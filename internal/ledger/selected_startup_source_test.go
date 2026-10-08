package ledger

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestSelectedStartupSourceOwnsActualCutAndScopeExpires(t *testing.T) {
	f := newSelectedStartupFixture(t, false)
	if _, err := f.native.Append(context.Background(), projectionTestLifecycle(2, true)[0]); err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	lease, err := f.native.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	head, hash, _ := f.native.ChainHead()
	prefix := ProjectionPrefix{Head: head, Hash: hash}
	o := f.ledgerOptions
	o.Prefix = &prefix
	selected := SelectedLedgerProjectionOpenOptions{Publisher: f.o.Publisher, Guard: f.o.Guard, Authority: f.o.Authority}
	p, _, err := OpenSelectedAuthenticatedLedgerProjection(context.Background(), f.native, f.ledgerDirectory, o, selected)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
		if err := f.ledgerOwner.AdoptClosedSuccessor(p); err != nil {
			t.Error(err)
		}
	}()
	if _, err := p.ReadReplayAnchor(context.Background()); !errors.Is(err, ErrStartupAuditPending) {
		t.Fatal("pending business read allowed", err)
	}
	var escaped SelectedStartupSource
	var escapedCtx context.Context
	if err := f.native.WithSelectedStartupOwner(context.Background(), o.Budget, selected, o.Identity, func(ctx context.Context, s SelectedStartupSource) error {
		escaped, escapedCtx = s, ctx
		if err := f.native.ValidateSelectedStartupRead(ctx); err != nil {
			return err
		}
		if s.SelectedSeal().RootCount != 2 {
			return ErrProjectionSource
		}
		if err := s.VerifyAnchors(ctx, [2]ReplayAnchor{s.SavedAnchor(), s.NativeHead()}); err != nil {
			return err
		}
		decoded := 0
		if _, err := s.ReplayAuthenticated(ctx, s.SavedAnchor(), func(Record) error { decoded++; return s.Check(ctx) }); err != nil {
			return err
		}
		if decoded != 1 {
			return ErrProjectionSource
		}
		if _, err := s.ReplayAuthenticated(ctx, ReplayAnchor{}, nil); !errors.Is(err, ErrProjectionCursor) {
			return ErrProjectionSource
		}
		if _, err := p.ReadReplayAnchor(ctx); err != nil {
			return err
		}
		_, err := p.SealReference(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := escaped.Check(context.Background()); err == nil {
		t.Fatal("escaped source allowed")
	}
	if err := f.native.ValidateSelectedStartupRead(escapedCtx); err == nil {
		t.Fatal("escaped scope revived grant")
	}
	if _, err := p.ReadReplayAnchor(escapedCtx); err == nil {
		t.Fatal("escaped scope granted point query")
	}
}

func TestSelectedLedgerQueriesAndSealDynamicallyFollowNativeGrant(t *testing.T) {
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
	a, err := f.native.startSelectedStartupAudit(context.Background(), o.Budget, StartupAuditOptions{Deadline: time.Second, ProgressTimeout: 100 * time.Millisecond}, func(r Record, _ *os.File) {
		if r.Sequence == 1 {
			close(entered)
			<-leave
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err := p.ReadReplayAnchor(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SealReference(context.Background()); !errors.Is(err, ErrStartupAuditPending) {
		t.Fatal("grant allowed premature publishing", err)
	}
	deadline := time.Now().Add(time.Second)
	for a.Snapshot().Ready && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, err := p.ReadReplayAnchor(context.Background()); err == nil {
		t.Fatal("expired actual grant still returned query")
	}
	close(leave)
	if err := a.Wait(context.Background()); err == nil {
		t.Fatal("expired audit accepted")
	}
}
