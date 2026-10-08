package bolt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/pricinglimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
	bbolt "go.etcd.io/bbolt"
)

func pricingBudgetFixture(t *testing.T) (*Store, *resourcebudget.Manager, time.Time) {
	t.Helper()
	l := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 128 << 20}, RecoveryReserve: resourcebudget.Cost{RAM: 8 << 20}, MaxClaims: 8, RecoveryClaims: 2, MaxOwners: 2, MaxPressureAge: time.Hour}
	l.Classes[resourcebudget.IO] = resourcebudget.Cost{RAM: pricinglimits.MinIORAMBytes}
	l.Classes[resourcebudget.Recovery] = resourcebudget.Cost{RAM: 4 << 20}
	l.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 8 * resourcebudget.ClaimRAMBytes}
	m, err := resourcebudget.New(l)
	if err != nil {
		t.Fatal(err)
	}
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	if err := s.ConfigurePricingResources(m, pricinglimits.MinLifetimeRAMBytes, pricinglimits.MinIORAMBytes); err != nil {
		t.Fatal(err)
	}
	seedPricingDeployment(t, s, "dep_budget", 0, 0, 0)
	base := time.Now().UTC().Add(-time.Minute)
	if _, err := s.CreateDeploymentPriceVersion(context.Background(), newStoredPrice("price_budget", "dep_budget", base)); err != nil {
		t.Fatal(err)
	}
	return s, m, base
}

func TestPricingResourcesUnknownRefusesBeforeClockAndNativePinThenHealthyRetries(t *testing.T) {
	s, m, base := pricingBudgetFixture(t)
	before, err := s.MetadataJournalState()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.PrepareDeploymentPricePin(context.Background(), "dep_budget", "att_budget", base, time.Second, time.Minute); err == nil {
		t.Fatal("unknown pressure admitted pricing IO")
	}
	if s.pricingClock != nil || m.Snapshot().NormalClaims != 0 || m.Snapshot().RecoveryClaims != 1 {
		t.Fatal("refusal allocated clock or lost lifetime owner", m.Snapshot())
	}
	after, err := s.MetadataJournalState()
	if err != nil || before.Sequence != after.Sequence {
		t.Fatal(before, after, err)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SelectDeploymentPriceVersion(context.Background(), "dep_budget", base); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.PrepareDeploymentPricePin(context.Background(), "dep_budget", "att_budget", base, time.Second, time.Minute); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().NormalClaims != 0 {
		t.Fatal("IO leaked after actual return", m.Snapshot())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("lifetime leaked", m.Snapshot())
	}
}

func TestPricingResourcesCancellationAndCloseKeepBlockedIOUntilActualExit(t *testing.T) {
	s, m, base := pricingBudgetFixture(t)
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	state := s.deploymentPricingState("dep_budget")
	state.timelineMu.Lock()
	locked := true
	defer func() {
		if locked {
			state.timelineMu.Unlock()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.SelectDeploymentPriceVersion(ctx, "dep_budget", base); done <- err }()
	deadline := time.Now().Add(time.Second)
	for m.Snapshot().NormalClaims != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.Snapshot().Classes[resourcebudget.IO].RAM != pricinglimits.MinIORAMBytes {
		t.Fatal("no admitted IO", m.Snapshot())
	}
	if _, err := s.SelectDeploymentPriceVersion(context.Background(), "other", base); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("parallel selection bypassed IO class", err)
	}
	cancel()
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		t.Fatal("Close refunded live selection", err)
	case <-time.After(20 * time.Millisecond):
	}
	if m.Snapshot().NormalClaims != 1 || m.Snapshot().RecoveryClaims != 1 {
		t.Fatal("cancel refunded actual work", m.Snapshot())
	}
	state.timelineMu.Unlock()
	locked = false
	if err := <-done; !errors.Is(err, context.Canceled) && !errors.Is(err, bbolt.ErrDatabaseNotOpen) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal(m.Snapshot())
	}
}

func TestPricingResourcesAuditCancelClosesScratchBeforeRefund(t *testing.T) {
	s, m, _ := pricingBudgetFixture(t)
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.auditDeploymentTimeline(ctx, "dep_budget"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(s.path), ".halro-price-audit-*"))
	if err != nil || len(paths) != 0 || m.Snapshot().NormalClaims != 0 {
		t.Fatal(paths, err, m.Snapshot())
	}
}

func TestPricingResourcesPhysicalCloseAndReplacementRefundBoundary(t *testing.T) {
	for _, fault := range []string{"close", "replacement"} {
		t.Run(fault, func(t *testing.T) {
			s, m, _ := pricingBudgetFixture(t)
			x, err := s.pricingClockOwner()
			if err != nil {
				t.Fatal(err)
			}
			if fault == "close" {
				if err := x.file.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Rename(x.dir, x.dir+".held"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(x.dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			err = s.Close()
			if fault == "close" {
				if err == nil || x.fileRetired || m.Snapshot().RecoveryClaims != 1 {
					t.Fatal("failed physical Close refunded owner", err, m.Snapshot())
				}
			} else {
				if !errors.Is(err, ownedfile.ErrReplaced) || !x.fileRetired || m.Snapshot().Used != (resourcebudget.Cost{}) {
					t.Fatal("closed RAM owner retained by unlink error", err, m.Snapshot())
				}
				if _, err := os.Stat(x.dir); err != nil {
					t.Fatal("removed replacement", err)
				}
			}
		})
	}
}

func TestPricingResourcesAuditFailedCloseRetainsIOAndLateConfigureRefuses(t *testing.T) {
	s, m, _ := pricingBudgetFixture(t)
	if err := s.ConfigurePricingResources(m, pricinglimits.MinLifetimeRAMBytes, pricinglimits.MinIORAMBytes); !errors.Is(err, resourcebudget.ErrInvalid) {
		t.Fatal(err)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	op, err := s.acquirePricingIO()
	if err != nil {
		t.Fatal(err)
	}
	x, err := newPriceAuditIndexWithRetirement(filepath.Join(t.TempDir(), "versions"), &op.retired)
	if err != nil {
		t.Fatal(err)
	}
	if err := x.file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := x.close(); err == nil || op.retired {
		t.Fatal("failed physical Close marked retired", err)
	}
	if err := op.release(); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().NormalClaims != 1 {
		t.Fatal("unproved retirement refunded IO", m.Snapshot())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().RecoveryClaims != 0 || m.Snapshot().NormalClaims != 1 {
		t.Fatal("Store Close erased orphan IO claim", m.Snapshot())
	}
}
