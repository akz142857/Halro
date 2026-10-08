package metadatastate

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/startupaudit"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func metadataGrantFixture(t *testing.T, options metadatajournal.StartupAuditOptions) (selectedMetadataNativeFixture, *Baseline, *boltstore.Store, *metadatajournal.StartupAudit, chan struct{}) {
	t.Helper()
	f := selectedMetadataNative(t)
	if err := f.baseline.Close(); err != nil {
		t.Fatal(err)
	}
	b, _ := f.reopenRoot(t)
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	a, err := b.StartSelectedStartupAuditObserved(context.Background(), options, func(ctx context.Context, records uint64, bytes int64) error {
		if records == 1 {
			<-release
		}
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	backend, err := b.SelectedStartupFacade(context.Background(), b.budget, b.evidence.Identity, b.evidence.InstanceID, b.evidence.KeyGeneration)
	if err != nil {
		t.Fatal(err)
	}
	s, err := boltstore.NewFacadeStore(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return f, b, s, a, release
}

func TestMetadataStartupGrantActualDAOAppendsWhileFixedPrefixAuditIsPending(t *testing.T) {
	f, b, s, a, release := metadataGrantFixture(t, metadatajournal.StartupAuditOptions{Deadline: 5 * time.Second, ProgressTimeout: 2 * time.Second})
	if p := a.Snapshot(); !p.Ready || p.Complete || p.Records != 1 || p.Bytes <= 0 {
		t.Fatal("no actual bounded epoch MAC progress", p)
	}
	if s.RecoveryError() != nil || b.journal.SelectedStartupStructuralReady() {
		t.Fatal("business/structural grants differ incorrectly", s.RecoveryError())
	}
	project := domain.Project{ID: "prj_grant", Name: "finite grant", Enabled: true}
	*f.rootReplaced = true
	got, err := s.PutProject(context.Background(), project, 0, nil)
	if err != nil {
		t.Fatal("same-inode DAO append refused", err)
	}
	if actual, err := s.GetProject(context.Background(), project.ID); err != nil || actual.ID != got.ID {
		t.Fatal(actual, err)
	}
	if _, err := b.SealReference(context.Background()); !errors.Is(err, metadatajournal.ErrStartupAuditPending) {
		t.Fatal("pending reader licensed Seal", err)
	}
	if _, err := b.TrimAtApplied(context.Background(), b.position); !errors.Is(err, metadatajournal.ErrStartupAuditPending) {
		t.Fatal("pending reader licensed trim", err)
	}
	if _, err := b.Snapshot(context.Background()); !errors.Is(err, metadatajournal.ErrStartupAuditPending) {
		t.Fatal("pending reader lent escaping raw snapshot", err)
	}
	if _, err := b.journal.InspectBoundContext(context.Background(), nil); !errors.Is(err, metadatajournal.ErrStartupAuditPending) {
		t.Fatal("pending reader admitted parallel full-history inspection", err)
	}
	if err := b.journal.ReplayRange(0, b.position.Sequence, nil); !errors.Is(err, metadatajournal.ErrStartupAuditPending) {
		t.Fatal("pending reader admitted parallel full-history replay", err)
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if !b.journal.SelectedStartupStructuralReady() || s.RecoveryError() != nil {
		t.Fatal("actual reader did not retire", a.Snapshot(), s.RecoveryError())
	}
	if _, err := b.SealReference(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataStartupGrantProgressTimeoutWithdrawsDAOWhileReaderFeeRemains(t *testing.T) {
	_, b, s, a, release := metadataGrantFixture(t, metadatajournal.StartupAuditOptions{Deadline: 2 * time.Second, ProgressTimeout: 300 * time.Millisecond})
	fee := b.budget.Snapshot().Used
	if fee.RAM < metadatajournal.FrozenReadRAMBytes {
		t.Fatal("actual reader missing fee", fee)
	}
	deadline := time.Now().Add(3 * time.Second)
	for a.Snapshot().Ready && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p := a.Snapshot(); p.Ready || !errors.Is(p.Err, startupaudit.ErrProgress) {
		t.Fatal("stalled audit kept grant", p)
	}
	if s.RecoveryError() == nil {
		t.Fatal("DAO health ignored withdrawn grant")
	}
	if _, err := s.GetProject(context.Background(), "absent"); err == nil || errors.Is(err, boltstore.ErrNotFound) {
		t.Fatal("withdrawn root masqueraded as 404", err)
	}
	if b.budget.Snapshot().Used != fee {
		t.Fatal("deadline refunded blocked reader", b.budget.Snapshot(), fee)
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		t.Fatal("DAO Close skipped actual reader exit", err)
	case <-time.After(25 * time.Millisecond):
	}
	if b.budget.Snapshot().Used.RAM < metadatajournal.FrozenReadRAMBytes {
		t.Fatal("reader fee refunded before exit")
	}
	close(release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Wait(ctx); !errors.Is(err, startupaudit.ErrProgress) && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if b.budget.Snapshot().Used.RAM >= metadatajournal.FrozenReadRAMBytes {
		t.Fatal("known reader exit did not return RAM", b.budget.Snapshot())
	}
}

func TestMetadataStartupGrantCallbackCannotCommitOrReadAfterActualCancel(t *testing.T) {
	f, b, _, a, release := metadataGrantFixture(t, metadatajournal.StartupAuditOptions{Deadline: 5 * time.Second, ProgressTimeout: 2 * time.Second})
	before := b.journal.Head()
	root, err := b.store.RootIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Update(context.Background(), func(tx *Transaction) error {
		if err := tx.Put([]string{"projects"}, []byte("staged"), []byte("v")); err != nil {
			return err
		}
		a.Cancel()
		if _, _, err := tx.Get([]string{"projects"}, []byte("staged")); err == nil {
			t.Fatal("callback read ignored cancellation")
		}
		return nil // ignored native refusal must remain sticky
	})
	if err == nil || b.journal.Head() != before {
		t.Fatal("withdrawn callback created a native fact", err)
	}
	actual, err := b.store.RootIdentity(context.Background())
	if err != nil || actual != root {
		t.Fatal("withdrawn callback published root", actual, err)
	}
	close(release)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if b.borrowedClean {
		t.Fatal("withdrawn/unfinished historical grant became a clean adoption receipt")
	}
	if err := f.baseline.AdoptClosedSuccessor(b); err == nil {
		t.Fatal("withdrawn candidate transferred creator retirement authority")
	}
}

func TestMetadataStartupGrantInvalidDeadlineRefusesBeforeFeeOrJob(t *testing.T) {
	f := selectedMetadataNative(t)
	if err := f.baseline.Close(); err != nil {
		t.Fatal(err)
	}
	b, _ := f.reopenRoot(t)
	fee := b.budget.Snapshot().Used
	for _, options := range []metadatajournal.StartupAuditOptions{{}, {Deadline: 16 * time.Minute, ProgressTimeout: time.Second}, {Deadline: time.Minute, ProgressTimeout: 31 * time.Second}} {
		if a, err := b.StartSelectedStartupAudit(context.Background(), options); a != nil || !errors.Is(err, startupaudit.ErrOptions) {
			t.Fatal(a, err)
		}
	}
	if b.ready.Load() || b.budget.Snapshot().Used != fee {
		t.Fatal("invalid deadline changed readiness/fee", resourcebudget.Cost(fee), b.budget.Snapshot())
	}
}

func TestMetadataStartupGrantActualOldTamperWithdrawsExistingDAO(t *testing.T) {
	_, b, s, a, release := metadataGrantFixture(t, metadatajournal.StartupAuditOptions{Deadline: 5 * time.Second, ProgressTimeout: 2 * time.Second})
	if s.RecoveryError() != nil {
		t.Fatal("actual initial progress did not grant DAO", s.RecoveryError())
	}
	raw, err := os.ReadFile(b.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-33] ^= 1
	if err := os.WriteFile(b.journalPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Wait(ctx); !errors.Is(err, metadatajournal.ErrCorrupt) {
		t.Fatal("actual old bytes escaped audit", err)
	}
	if s.RecoveryError() == nil || b.journal.SelectedStartupStructuralReady() {
		t.Fatal("failed reader left existing DAO healthy")
	}
	if _, err := s.GetProject(context.Background(), "absent"); err == nil || errors.Is(err, boltstore.ErrNotFound) {
		t.Fatal("old byte failure became an absent business row", err)
	}
	if _, err := b.SealReference(context.Background()); err == nil {
		t.Fatal("failed historical audit exported a Seal")
	}
}

type withdrawAfterMissingBackend struct {
	boltstore.FacadeBackend
	audit *metadatajournal.StartupAudit
}

func (f withdrawAfterMissingBackend) View(ctx context.Context, visit func(boltstore.FacadeTransaction) error) error {
	err := f.FacadeBackend.View(ctx, visit)
	if errors.Is(err, boltstore.ErrNotFound) {
		f.audit.Cancel()
	}
	return err
}

func TestMetadataStartupGrantWithdrawalAfterActualMissingReadNeverReturns404(t *testing.T) {
	_, b, _, a, release := metadataGrantFixture(t, metadatajournal.StartupAuditOptions{Deadline: 5 * time.Second, ProgressTimeout: 2 * time.Second})
	backend, err := b.SelectedStartupFacade(context.Background(), b.budget, b.evidence.Identity, b.evidence.InstanceID, b.evidence.KeyGeneration)
	if err != nil {
		t.Fatal(err)
	}
	s, err := boltstore.NewFacadeStore(context.Background(), withdrawAfterMissingBackend{backend, a})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	// The actual authenticated callback has already returned a valid missing
	// row when native cancellation occurs. Only the wrapper sets this exact
	// ordering; health and every source/read remain the real selected backend.
	if _, err := s.GetProject(context.Background(), "missing"); err == nil || errors.Is(err, boltstore.ErrNotFound) || !errors.Is(err, boltstore.ErrMetadataRecoveryRequired) {
		t.Fatal("native withdrawal raced into a false 404", err)
	}
	close(release)
}
