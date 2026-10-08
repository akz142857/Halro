package metadatastate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func metadataPhysicalSelected(t *testing.T) (selectedMetadataNativeFixture, *Baseline) {
	t.Helper()
	f := selectedMetadataNativeCountRetirement(t, 0, true)
	if err := f.baseline.Close(); err != nil {
		t.Fatal(err)
	}
	b, _ := f.reopenRoot(t)
	if err := b.AuditSelectedRootStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f, b
}
func physicalPin(t *testing.T) domain.PricePinIntent {
	t.Helper()
	now := time.Now().UTC()
	p := domain.PricePinIntent{AttemptID: "att_terminal", DeploymentID: "dep_terminal", PriceVersionID: "prv_terminal", PriceVersion: 1, SnapshotSHA256: fmtSHA256([]byte("snapshot")), PricingSelectedAt: now, MetadataRevision: 1, State: domain.PricePinPrepared, CreatedAt: now}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return p
}
func fmtSHA256(b []byte) string {
	h := sha256.Sum256(b)
	const x = "0123456789abcdef"
	s := make([]byte, 64)
	for i, c := range h {
		s[i*2] = x[c>>4]
		s[i*2+1] = x[c&15]
	}
	return "sha256:" + string(s)
}

func TestMetadataPhysicalNativeLoanAuthenticatesActualPrefixAndRevokesEscape(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("actual Linux inode identity")
	}
	f, b := metadataPhysicalSelected(t)
	ctx := context.Background()
	var escaped *metadatajournal.PhysicalNativeLoan
	if err := b.journal.WithPhysicalNative(ctx, b.budget, func(q *metadatajournal.PhysicalNativeLoan) error {
		escaped = q
		ref, err := q.Reference()
		if err != nil {
			return err
		}
		head, last, err := q.HeadAt(ctx, 0)
		if err != nil || head != b.position || last.Sequence != 0 {
			return errors.Join(ErrSource, err)
		}
		if err = q.AuthenticatePrefix(ctx, ref); err != nil {
			return err
		}
		ref.Hash[0] ^= 1
		if err = q.AuthenticatePrefix(ctx, ref); !errors.Is(err, metadatajournal.ErrCorrupt) {
			t.Fatal("forged prefix admitted", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := escaped.Reference(); !errors.Is(err, authenticatedstate.ErrClosed) {
		t.Fatal("escaped source loan", err)
	}
	if err := b.journal.WithPhysicalNative(ctx, nil, func(*metadatajournal.PhysicalNativeLoan) error {
		t.Fatal("foreign Manager callback entered")
		return nil
	}); err == nil {
		t.Fatal("foreign manager admitted")
	}
	// No physical allocation was requested; Close must still join the actual parent.
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	*f.rootReplaced = true
}
func TestMetadataPhysicalPricePinTransitionUsesAuthenticatedPriorIntent(t *testing.T) {
	f, b := metadataPhysicalSelected(t)
	*f.rootReplaced = true
	ctx := context.Background()
	p := physicalPin(t)
	raw, _ := json.Marshal(p)
	if _, err := b.Update(ctx, func(tx *Transaction) error {
		return tx.Put([]string{"deployment_price_pin_intents"}, []byte(p.AttemptID), raw)
	}); err != nil {
		t.Fatal(err)
	}
	committed := p
	at := time.Now().UTC()
	committed.State = domain.PricePinCommitted
	committed.LedgerSequence = 7
	committed.CommittedAt = &at
	value, _ := json.Marshal(committed)
	ops := []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"deployment_price_pin_intents"}, Key: []byte(p.AttemptID), Value: value}}
	if err := b.validatePhysicalPricePinLocked(ctx, ops); err != nil {
		t.Fatal(err)
	}
	committed.DeploymentID = "dep_foreign"
	ops[0].Value, _ = json.Marshal(committed)
	if err := b.validatePhysicalPricePinLocked(ctx, ops); !errors.Is(err, ErrSource) {
		t.Fatal("changed prior evidence admitted", err)
	}
	ref, err := b.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := b.StandaloneFacade(ctx, b.budget, b.evidence.Identity, b.evidence.InstanceID, b.evidence.KeyGeneration, ref)
	if err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.NewFacadeStore(ctx, backend)
	if err != nil {
		t.Fatal(err)
	}
	// The real facade DAO preserves cancellation before changing native facts.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	before := b.journal.Head()
	if _, err := store.CommitDeploymentPricePin(canceled, p.AttemptID, p.SnapshotSHA256, 7, at); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if b.journal.Head() != before {
		t.Fatal("canceled commit changed source")
	}
	if _, err := store.CommitDeploymentPricePin(ctx, p.AttemptID, p.SnapshotSHA256, 7, at); err != nil {
		t.Fatal(err)
	}
}

// This portable test isolates the actual resource continuation through real
// native/COW Facade operations. Physical extents are exercised separately by
// the Linux ext4 test; the manual pressure boundary is not an OS capacity claim.
func TestMetadataPhysicalFacadePrepaidBudgetSurvivesPressureAndCancellation(t *testing.T) {
	f, b := metadataPhysicalSelected(t)
	*f.rootReplaced = true
	ctx := context.Background()
	p := physicalPin(t)
	raw, _ := json.Marshal(p)
	if _, err := b.Update(ctx, func(tx *Transaction) error {
		return tx.Put([]string{"deployment_price_pin_intents"}, []byte(p.AttemptID), raw)
	}); err != nil {
		t.Fatal(err)
	}
	ref, err := b.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := b.StandaloneFacade(ctx, b.budget, b.evidence.Identity, b.evidence.InstanceID, b.evidence.KeyGeneration, ref)
	if err != nil {
		t.Fatal(err)
	}
	dao, err := boltstore.NewFacadeStore(ctx, backend)
	if err != nil {
		t.Fatal(err)
	}
	work, err := b.acquirePhysicalCompletion()
	if err != nil {
		t.Fatal(err)
	}
	w := &TerminalPhysicalWindow{owner: b, work: work.Active, completion: work}
	defer func() {
		if err := work.Close(); err != nil {
			t.Error(err)
		}
		if err := work.Active.Release(); err != nil {
			t.Error(err)
		}
		b.budget.SetPressure(resourcebudget.Healthy)
	}()
	if err = b.budget.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	if _, err = backend.Update(ctx, func(boltstore.FacadeTransaction) error { t.Fatal("ordinary callback admitted"); return nil }); !errors.Is(err, resourcebudget.ErrPressure) {
		t.Fatal(err)
	}
	before := b.journal.Head()
	caller, cancel := context.WithCancel(w.Bind(ctx))
	started := make(chan struct{})
	resume := make(chan struct{})
	done := make(chan error, 1)
	terminal := backend.(interface {
		PricePinTerminalContext(context.Context) context.Context
	}).PricePinTerminalContext(caller)
	go func() {
		_, e := backend.Update(terminal, func(tx boltstore.FacadeTransaction) error {
			close(started)
			<-resume
			return tx.Put([]string{"deployment_price_pin_intents"}, []byte(p.AttemptID), raw)
		})
		done <- e
	}()
	<-started
	cancel()
	close(resume)
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("canceled native accepted", err)
	}
	if b.journal.Head() != before {
		t.Fatal("canceled terminal changed native")
	}
	if _, err = dao.CommitDeploymentPricePin(w.Bind(ctx), p.AttemptID, p.SnapshotSHA256, 7, time.Now().UTC()); err != nil {
		t.Fatal("prepaid real Facade commit", err)
	}
	if b.journal.Head().Sequence != before.Sequence+1 {
		t.Fatal("terminal not durable")
	}
}

func metadataPhysicalBudget(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	// Functional owner fixture: terminal credits plus simultaneous ordinary
	// preparation are charged; this does not change Runtime/capacity limits.
	l := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 512 << 20, Disk: 512 << 20}, MaxClaims: 64, MaxOwners: 8, MaxPressureAge: time.Hour}
	for i := range l.Classes {
		l.Classes[i] = l.Total
	}
	l.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	l.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 64 * resourcebudget.ClaimRAMBytes}
	m, err := resourcebudget.New(l)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	return m
}
