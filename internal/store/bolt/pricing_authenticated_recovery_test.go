package bolt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"path/filepath"
	"testing"
	"time"
)

func TestPricePinRecoveryUsesAuthenticatedCandidateAcrossPages(t *testing.T) {
	s, snapshot := pricingRecoveryFixture(t, 129)
	path := filepath.Join(t.TempDir(), "ledger.wal")
	key := bytes.Repeat([]byte{0x71}, 32)
	l, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	expected := make(map[string]uint64)
	for i := 0; i < 129; i += 2 {
		base := ledger.Event{EventID: fmt.Sprint("accepted_", i), Kind: ledger.EventRequestAccepted, RequestID: fmt.Sprint("req_", i), ProjectID: "prj_recovery", PeriodID: "2026-10-03", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: snapshot.PricingSelectedAt}
		if _, err := l.Append(context.Background(), base); err != nil {
			t.Fatal(err)
		}
		base.Kind, base.EventID, base.AttemptID = ledger.EventReservationCreated, fmt.Sprint("lease_", i), fmt.Sprintf("att_%03d", i)
		base.ReservationMicrosUSD, base.LeaseMode, base.PriceSnapshot = ledger.MicrosUSD(1), ledger.LeaseModeMetered, &snapshot
		base.TokenGuardPricingViewDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		record, _, err := l.AppendRecordWithReceipt(context.Background(), base)
		if err != nil {
			t.Fatal(err)
		}
		expected[base.AttemptID] = record.Sequence
	}
	lease, err := l.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var p *ledger.AuthenticatedLedgerProjection
	if err := lease.WithHead(context.Background(), func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		report, partial, err := ledger.InspectReplayAuthenticated(path, key, nil)
		if err != nil || partial || report.Head != head || report.ChainHash != hash {
			return errors.Join(err, errors.New("fixture authentication mismatch"))
		}
		limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 256 << 20, Disk: 256 << 20}, MaxClaims: 16, MaxOwners: 4, MaxPressureAge: time.Hour}
		for i := range limits.Classes {
			limits.Classes[i] = limits.Total
		}
		limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
		limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 16 * resourcebudget.ClaimRAMBytes}
		budget, budgetErr := resourcebudget.New(limits)
		if budgetErr != nil {
			return budgetErr
		}
		if err := budget.SetPressure(resourcebudget.Healthy); err != nil {
			return err
		}
		p, err = ledger.BuildAuthenticatedLedgerProjection(ctx, path, key, filepath.Join(t.TempDir(), "candidate"), report, ledger.AuthenticatedLedgerOptions{Identity: sha256.Sum256([]byte("independent-native-fixture")), MaxDiskBytes: 128 << 20, MaxAppendBytes: 8 << 20, Budget: budget})
		t.Cleanup(func() {
			if p != nil {
				if err := p.Discard(); err != nil {
					t.Error(err)
				}
			}
			if used := budget.Snapshot().Used; used != (resourcebudget.Cost{}) {
				t.Error("fee leak", used)
			}
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// Acquiring a metadata writer must be possible inside the external Ledger
	// callback; a metadata transaction retained across the callback would block.
	reader := pricingRecoveryReader{AccountingLeaseReader: p, lookup: func(ctx context.Context, id string) (ledger.Record, bool, error) {
		probe := make(chan error, 1)
		go func() {
			tx, err := s.db.Begin(true)
			if err == nil {
				err = tx.Rollback()
			}
			probe <- err
		}()
		select {
		case err := <-probe:
			if err != nil {
				return ledger.Record{}, false, err
			}
		case <-time.After(time.Second):
			return ledger.Record{}, false, errors.New("metadata writer held across Ledger lookup")
		}
		return p.ReadLease(ctx, id)
	}}
	if err := s.RecoverDeploymentPricePins(context.Background(), reader); err != nil {
		t.Fatal(err)
	}
	pins := pricingRecoveryPins(t, s)
	if len(pins) != len(expected) {
		t.Fatal("pin recovery lost facts or retained orphans", len(pins), len(expected))
	}
	for id, seq := range expected {
		pin, found := pins[id]
		if !found || pin.State != domain.PricePinCommitted || pin.LedgerSequence != seq {
			t.Fatal(id, pin, seq)
		}
	}
	before, err := s.PricingBackupState()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverDeploymentPricePins(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	after, err := s.PricingBackupState()
	if err != nil || before != after {
		t.Fatal("recovery retry changed committed facts", before, after, err)
	}
}
