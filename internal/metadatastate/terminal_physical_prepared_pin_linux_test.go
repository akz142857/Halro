//go:build linux

package metadatastate

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func TestMetadataPhysicalPreparedObligationRefusesForeignWindowAndRetainsClose(t *testing.T) {
	requireMetadataPhysicalExt4(t)
	f := selectedMetadataNativeCountRetirementPolicy(t, 0, true, true)
	*f.rootReplaced = true
	ctx := context.Background()
	if err := f.baseline.Close(); err != nil {
		t.Fatal(err)
	}
	rc, oldJournal, err := f.baseline.DiskReservations()
	if err != nil {
		t.Fatal(err)
	}
	if err := oldJournal.Release(); err != nil {
		t.Fatal(err)
	}
	jc, err := f.options.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	o := SelectedOpenOptions{BaselineOptions: baselineOptionsForSelectedFixture(f), Evidence: f.baseline.evidence, Binding: f.options.Binding, Publisher: f.options.Publisher, Guard: f.options.Guard, Authority: f.options.Authority, RootDiskReservation: rc, JournalDiskReservation: jc, MaxJournalBytes: 64 << 20}
	b, _, err := OpenSelectedFromRoot(ctx, metadataTestKey(), f.baseline.output, f.path, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AuditSelectedRootStartup(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.EnableQualifiedPhysicalTerminals(ctx, f.options.Publisher, f.options.Guard, f.options.Authority); err != nil {
		t.Fatal(err)
	}
	w, err := b.ReservePhysicalTerminals(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	p := physicalPin(t)
	raw, _ := json.Marshal(p)
	before := b.journal.Head()
	put := func(tx *Transaction) error {
		return tx.Put([]string{"deployment_price_pin_intents"}, []byte(p.AttemptID), raw)
	}
	if _, err := b.Update(ctx, put); !errors.Is(err, resourcebudget.ErrPhysicalAllocation) || b.journal.Head() != before {
		t.Fatal("Prepared without original window admitted", err)
	}
	if _, err := b.Update(w.Bind(ctx), put); err != nil {
		t.Fatal(err)
	}
	if !w.hasPreparedPins() || w.remaining != 1 {
		t.Fatal("durable Prepared lost original obligation")
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
	foreign, err := b.ReservePhysicalTerminals(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	before = b.journal.Head()
	if _, err := b.Update(foreign.Bind(ctx), put); !errors.Is(err, ErrSource) || b.journal.Head() != before {
		t.Fatal("foreign Prepared rewrite borrowed original obligation", err)
	}
	if err := dao.DeletePreparedDeploymentPricePin(foreign.Bind(ctx), p.AttemptID); !errors.Is(err, ErrSource) || b.journal.Head() != before {
		t.Fatal("foreign window consumed original obligation", err)
	}
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(w.Bind(ctx))
	cancel()
	if err := dao.DeletePreparedDeploymentPricePin(canceled, p.AttemptID); !errors.Is(err, context.Canceled) || !w.hasPreparedPins() || b.journal.Head() != before {
		t.Fatal("canceled cleanup lost Prepared obligation", err)
	}
	fees := b.budget.Snapshot().Used
	if err := w.Close(); !errors.Is(err, authenticatedstate.ErrRecovery) {
		t.Fatal("Prepared close retired targets", err)
	}
	if !w.hasPreparedPins() || w.remaining != 1 || b.physicalDebt != 1 || b.ready.Load() {
		t.Fatal("close lost native/COW obligation")
	}
	if b.budget.Snapshot().Used != fees {
		t.Fatal("unknown Prepared close refunded original fees")
	}
	if err := w.Close(); !errors.Is(err, authenticatedstate.ErrRecovery) {
		t.Fatal("second close lost debt", err)
	}
	if err := b.Close(); !errors.Is(err, authenticatedstate.ErrRecovery) {
		t.Fatal("owner close lost debt", err)
	}
	if b.budget.Snapshot().Used != fees {
		t.Fatal("actual owner close guessed fee refund")
	}
}
