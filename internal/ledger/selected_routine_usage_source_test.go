package ledger

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestSelectedRoutineUsageSourceUsesReadGrantAndSavedSuffix(t *testing.T) {
	f := newSelectedStartupFixture(t, false)
	f.reopen(t)
	lease, err := f.native.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o := f.ledgerOptions
	prefix := ProjectionPrefix{Head: o.SavedAnchor.Head, Hash: o.SavedAnchor.Hash}
	o.Prefix = &prefix
	p, _, err := OpenSelectedAuthenticatedLedgerProjection(context.Background(), f.native, f.ledgerDirectory, o, SelectedLedgerProjectionOpenOptions{Publisher: f.o.Publisher, Guard: f.o.Guard, Authority: f.o.Authority})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	entered, leave := make(chan struct{}), make(chan struct{})
	a, err := f.native.startSelectedStartupAudit(context.Background(), o.Budget, StartupAuditOptions{Deadline: time.Second, ProgressTimeout: time.Second}, func(r Record, _ *os.File) {
		if r.Sequence == 1 {
			close(entered)
			<-leave
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	ref, err := p.ReadUsageSourceReference(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.SealReference(context.Background()); !errors.Is(err, ErrStartupAuditPending) {
		t.Fatal(err)
	}
	if err := f.native.ValidateStandaloneReadSource(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Damage an unvisited old frame. The actual routine source must read only
	// the selected saved suffix; the separate auditor will still reject this.
	file, err := os.OpenFile(f.path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	pos := f.native.startupSaved.Head.Offset - 1
	if _, err := file.ReadAt(b[:], pos); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 1
	if _, err := file.WriteAt(b[:], pos); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.WithFrozenUsageSource(context.Background(), f.native, o.Budget, ref.Scope, func(ctx context.Context, s AuthenticatedUsageSource) error {
		if s.Reference() != ref {
			return ErrProjectionSource
		}
		return s.VerifyAnchors(ctx, [2]ReplayAnchor{ref.Anchor, s.NativeHead()})
	}); err != nil {
		t.Fatal("routine source scanned genesis or required structural publication", err)
	}
	close(leave)
	if err := a.Wait(context.Background()); err == nil {
		t.Fatal("separate historical auditor missed corruption")
	}
	if _, err := p.ReadUsageSourceReference(context.Background()); err == nil {
		t.Fatal("routine reference survived native corruption")
	}
}

func TestSelectedRoutineUsageSourceExpiryCancelsCallbackKeepsWholeJobUntilExit(t *testing.T) {
	f := newSelectedStartupFixture(t, false)
	f.reopen(t)
	lease, err := f.native.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o := f.ledgerOptions
	prefix := ProjectionPrefix{Head: o.SavedAnchor.Head, Hash: o.SavedAnchor.Hash}
	o.Prefix = &prefix
	p, _, err := OpenSelectedAuthenticatedLedgerProjection(context.Background(), f.native, f.ledgerDirectory, o, SelectedLedgerProjectionOpenOptions{Publisher: f.o.Publisher, Guard: f.o.Guard, Authority: f.o.Authority})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	auditEntered, auditLeave := make(chan struct{}), make(chan struct{})
	a, err := f.native.startSelectedStartupAudit(context.Background(), o.Budget, StartupAuditOptions{Deadline: time.Second, ProgressTimeout: 100 * time.Millisecond}, func(r Record, _ *os.File) {
		if r.Sequence == 1 {
			close(auditEntered)
			<-auditLeave
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-auditEntered
	ref, err := p.ReadUsageSourceReference(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entered, canceled, leave := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- p.WithFrozenUsageSource(context.Background(), f.native, o.Budget, ref.Scope, func(ctx context.Context, s AuthenticatedUsageSource) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-leave
			return s.Check(ctx)
		})
	}()
	<-entered
	used := o.Budget.Snapshot().Used
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("actual finite grant did not cancel routine read callback")
	}
	if got := o.Budget.Snapshot().Used; got != used {
		t.Fatal("live canceled IO refunded", got, used)
	}
	close(leave)
	if err := <-done; err == nil {
		t.Fatal("expired source returned success")
	}
	close(auditLeave)
	if err := a.Wait(context.Background()); err == nil {
		t.Fatal("expired audit accepted")
	}
}
