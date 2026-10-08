package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

func pricingRecoveryFixture(t *testing.T, count int) (*Store, domain.PriceSnapshot) {
	t.Helper()
	s, err := openForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	seedPricingDeployment(t, s, "dep_recovery", 0, 0, 0)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	if _, err := s.CreateDeploymentPriceVersion(context.Background(), newStoredPrice("price_recovery", "dep_recovery", now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	var snapshot domain.PriceSnapshot
	for i := 0; i < count; i++ {
		_, selected, _, err := s.PrepareDeploymentPricePin(context.Background(), "dep_recovery", fmt.Sprintf("att_%03d", i), now, time.Second, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		snapshot = selected
	}
	return s, snapshot
}

func pricingRecoveryPins(t *testing.T, s *Store) map[string]domain.PricePinIntent {
	t.Helper()
	result := make(map[string]domain.PricePinIntent)
	if err := s.view(func(tx *Tx) error {
		return tx.Bucket(bucketDeploymentPricePins).ForEach(func(key, raw []byte) error {
			var pin domain.PricePinIntent
			if err := json.Unmarshal(raw, &pin); err != nil {
				return err
			}
			result[string(key)] = pin
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

type pricingRecoveryReader struct {
	ledger.AccountingLeaseReader
	lookup   func(context.Context, string) (ledger.Record, bool, error)
	position func(context.Context) (ledger.Watermark, [32]byte, error)
}

func (r pricingRecoveryReader) ReadLease(ctx context.Context, id string) (ledger.Record, bool, error) {
	if r.lookup != nil {
		return r.lookup(ctx, id)
	}
	return r.AccountingLeaseReader.ReadLease(ctx, id)
}
func (r pricingRecoveryReader) ReadPosition(ctx context.Context) (ledger.Watermark, [32]byte, error) {
	if r.position != nil {
		return r.position(ctx)
	}
	return r.AccountingLeaseReader.ReadPosition(ctx)
}

func TestPricePinRecoveryUsesFreshDiskProjectionAcrossPages(t *testing.T) {
	s, snapshot := pricingRecoveryFixture(t, 145)
	path := filepath.Join(t.TempDir(), "ledger.wal")
	key := bytes.Repeat([]byte{0x71}, 32)
	l, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	expected := make(map[string]uint64)
	for i := 0; i < 145; i += 2 {
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
	var p *ledger.LedgerProjection
	if err := lease.WithHead(context.Background(), func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		report, partial, err := ledger.InspectReplayAuthenticated(path, key, nil)
		if err != nil || partial || report.Head != head || report.ChainHash != hash {
			return errors.Join(err, errors.New("fixture authentication mismatch"))
		}
		p, err = ledger.BuildLedgerProjection(ctx, path, key, filepath.Join(t.TempDir(), "projection.db"), report)
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

func TestPricePinRecoveryReaderErrorsAndCancellationPreservePrepared(t *testing.T) {
	for _, fault := range []string{"read_error", "cancel", "position_changed", "position_error", "closed_projection", "nil_state", "nil_projection"} {
		t.Run(fault, func(t *testing.T) {
			s, _ := pricingRecoveryFixture(t, 2)
			before := pricingRecoveryPins(t, s)
			state := ledger.NewState()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			injected := errors.New("injected accounting disk read error")
			var looked atomic.Bool
			reader := ledger.AccountingLeaseReader(pricingRecoveryReader{AccountingLeaseReader: state, lookup: func(ctx context.Context, id string) (ledger.Record, bool, error) {
				looked.Store(true)
				if fault == "cancel" {
					cancel()
					return ledger.Record{}, false, ctx.Err()
				}
				if fault == "read_error" {
					return ledger.Record{}, false, injected
				}
				return state.ReadLease(ctx, id)
			}, position: func(ctx context.Context) (ledger.Watermark, [32]byte, error) {
				if looked.Load() && fault == "position_changed" {
					return ledger.Watermark{Sequence: 1}, [32]byte{}, nil
				}
				if looked.Load() && fault == "position_error" {
					return ledger.Watermark{}, [32]byte{}, injected
				}
				return state.ReadPosition(ctx)
			}})
			if fault == "nil_state" {
				var absent *ledger.State
				reader = absent
			}
			if fault == "nil_projection" {
				var absent *ledger.LedgerProjection
				reader = absent
			}
			if fault == "closed_projection" {
				path := filepath.Join(t.TempDir(), "ledger.wal")
				key := bytes.Repeat([]byte{0x72}, 32)
				l, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
				if err != nil {
					t.Fatal(err)
				}
				l.Close()
				report, partial, err := ledger.InspectReplayAuthenticated(path, key, nil)
				if err != nil || partial {
					t.Fatal(err)
				}
				p, err := ledger.BuildLedgerProjection(ctx, path, key, filepath.Join(t.TempDir(), "p.db"), report)
				if err != nil {
					t.Fatal(err)
				}
				p.Close()
				reader = p
			}
			if err := s.RecoverDeploymentPricePins(ctx, reader); err == nil {
				t.Fatal("failed read interpreted as absent lease")
			}
			after := pricingRecoveryPins(t, s)
			left, _ := json.Marshal(before)
			right, _ := json.Marshal(after)
			if !bytes.Equal(left, right) {
				t.Fatal("failed recovery deleted or changed prepared facts")
			}
		})
	}
}

func TestPricePinRecoveryCommitsOnlyCompletePagesAndRetries(t *testing.T) {
	for _, failAt := range []int{12, 65} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			s, _ := pricingRecoveryFixture(t, 70)
			state := ledger.NewState()
			reads := 0
			injected := errors.New("point read failure")
			r := pricingRecoveryReader{AccountingLeaseReader: state, lookup: func(ctx context.Context, id string) (ledger.Record, bool, error) {
				reads++
				if reads == failAt {
					return ledger.Record{}, false, injected
				}
				return state.ReadLease(ctx, id)
			}}
			if err := s.RecoverDeploymentPricePins(context.Background(), r); !errors.Is(err, injected) {
				t.Fatal(err)
			}
			want := 70
			if failAt == 65 {
				want = 6
			}
			if len(pricingRecoveryPins(t, s)) != want {
				t.Fatal("partial page committed or complete prefix rolled back")
			}
			if err := s.RecoverDeploymentPricePins(context.Background(), state); err != nil {
				t.Fatal(err)
			}
			if len(pricingRecoveryPins(t, s)) != 0 {
				t.Fatal("retry skipped remaining orphan pins")
			}
		})
	}
}

func TestPricePinRecoveryDetectsMetadataInsertionBehindCursor(t *testing.T) {
	s, _ := pricingRecoveryFixture(t, 70)
	state := ledger.NewState()
	changed := false
	r := pricingRecoveryReader{AccountingLeaseReader: state, lookup: func(ctx context.Context, id string) (ledger.Record, bool, error) {
		if !changed {
			changed = true
			if err := s.update(func(tx *Tx) error {
				b := tx.Bucket(bucketDeploymentPricePins)
				var pin domain.PricePinIntent
				if err := json.Unmarshal(b.Get([]byte("att_000")), &pin); err != nil {
					return err
				}
				pin.AttemptID = "aaa_new"
				raw, err := json.Marshal(pin)
				if err != nil {
					return err
				}
				return b.Put([]byte(pin.AttemptID), raw)
			}); err != nil {
				return ledger.Record{}, false, err
			}
		}
		return state.ReadLease(ctx, id)
	}}
	if err := s.RecoverDeploymentPricePins(context.Background(), r); !errors.Is(err, ErrPricingRecoveryChanged) {
		t.Fatal("metadata race not detected", err)
	}
	if len(pricingRecoveryPins(t, s)) != 71 {
		t.Fatal("concurrent mutation credited or selected page partly deleted")
	}
	if err := s.RecoverDeploymentPricePins(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if len(pricingRecoveryPins(t, s)) != 0 {
		t.Fatal("retry skipped insertion")
	}
}

func TestPricePinRecoveryBoundedRowsAndBytes(t *testing.T) {
	s, _ := pricingRecoveryFixture(t, 70)
	page, err := s.readPricingRecoveryPage(context.Background(), bucketDeploymentPricePins, nil, 0)
	if err != nil || page.done || len(page.rows) != pricingRecoveryRows {
		t.Fatal(page.done, len(page.rows), err)
	}
	// Simulate malformed physical DB content beyond the native journal limit.
	// This deliberately bypasses the journal only as a corruption fixture.
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketDeploymentPricePins).Put([]byte("aaa_oversized"), bytes.Repeat([]byte{'x'}, pricingRecoveryWideRowBytes+1))
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverDeploymentPricePins(context.Background(), ledger.NewState()); !errors.Is(err, ErrPricingRecoveryLimit) {
		t.Fatal("oversized row decoded or silently skipped", err)
	}
	if err := s.view(func(tx *Tx) error {
		if tx.Bucket(bucketDeploymentPricePins).Get([]byte("aaa_oversized")) == nil {
			return errors.New("oversized row discarded")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPricingRecoveryPreservesValidWideLegacyRowsAfterReopen(t *testing.T) {
	s, _ := pricingRecoveryFixture(t, 1)
	// This field is legal in the old schema and fits a real native metadata
	// transaction. It exceeds the ordinary page envelope, not the format cap.
	if err := s.update(func(tx *Tx) error {
		b := tx.Bucket(bucketDeploymentPricingHighWater)
		var high domain.DeploymentPricingHighWater
		if err := json.Unmarshal(b.Get([]byte("dep_recovery")), &high); err != nil {
			return err
		}
		high.Quarantined, high.QuarantineReason = true, strings.Repeat("reason", pricingRecoveryBytes/6+16)
		if err := high.Validate(); err != nil {
			return err
		}
		raw, err := json.Marshal(high)
		if err != nil {
			return err
		}
		if err := b.Put([]byte(high.DeploymentID), raw); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	path := s.db.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openForTest(t, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	page, err := reopened.readPricingRecoveryPage(context.Background(), bucketDeploymentPricingHighWater, nil, 0)
	if err != nil || len(page.rows) != 1 || len(page.rows[0].key)+len(page.rows[0].raw) <= pricingRecoveryBytes {
		t.Fatal("wide high-water not processed alone", len(page.rows), err)
	}
	var original []byte
	if err := reopened.view(func(tx *Tx) error {
		original = bytes.Clone(tx.Bucket(bucketDeploymentPricingHighWater).Get([]byte("dep_recovery")))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(original) <= pricingRecoveryBytes {
		t.Fatal("fixture does not exceed ordinary page")
	}
	if err := reopened.RecoverDeploymentPricePins(context.Background(), ledger.NewState()); err != nil {
		t.Fatal("valid legacy row rejected", err)
	}
	if len(pricingRecoveryPins(t, reopened)) != 0 {
		t.Fatal("normal orphan skipped")
	}
	if err := reopened.view(func(tx *Tx) error {
		if !bytes.Equal(original, tx.Bucket(bucketDeploymentPricingHighWater).Get([]byte("dep_recovery"))) {
			return errors.New("wide quarantined history rewritten")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPricingHighWaterRecoveryAcrossPagesAndFailurePrefix(t *testing.T) {
	s, _ := pricingRecoveryFixture(t, 1)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	if err := s.update(func(tx *Tx) error {
		for i := 0; i < 130; i++ {
			high := domain.DeploymentPricingHighWater{DeploymentID: fmt.Sprintf("aaa_dep_%03d", i), LatestObservedPriceVersionID: "missing", LatestSelectedAt: now, LatestObservedEffectiveFrom: now.Add(-time.Hour), Revision: 1}
			raw, err := json.Marshal(high)
			if err != nil {
				return err
			}
			if i == 64 {
				raw = []byte("invalid")
			}
			if err := tx.Bucket(bucketDeploymentPricingHighWater).Put([]byte(high.DeploymentID), raw); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverDeploymentPricePins(context.Background(), ledger.NewState()); err == nil {
		t.Fatal("invalid high-water decoded successfully")
	}
	if err := s.view(func(tx *Tx) error {
		var high domain.DeploymentPricingHighWater
		for _, id := range []int{0, 63, 65, 129} {
			if err := json.Unmarshal(tx.Bucket(bucketDeploymentPricingHighWater).Get([]byte(fmt.Sprintf("aaa_dep_%03d", id))), &high); err != nil {
				return err
			}
			want := id < 64
			if high.Quarantined != want {
				return fmt.Errorf("wrong quarantine prefix at %d", id)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.update(func(tx *Tx) error {
		high := domain.DeploymentPricingHighWater{DeploymentID: "aaa_dep_064", LatestObservedPriceVersionID: "missing", LatestSelectedAt: now, LatestObservedEffectiveFrom: now.Add(-time.Hour), Revision: 1}
		raw, err := json.Marshal(high)
		if err != nil {
			return err
		}
		return tx.Bucket(bucketDeploymentPricingHighWater).Put([]byte(high.DeploymentID), raw)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverDeploymentPricePins(context.Background(), ledger.NewState()); err != nil {
		t.Fatal(err)
	}
	if err := s.view(func(tx *Tx) error {
		for i := 0; i < 130; i++ {
			var high domain.DeploymentPricingHighWater
			if err := json.Unmarshal(tx.Bucket(bucketDeploymentPricingHighWater).Get([]byte(fmt.Sprintf("aaa_dep_%03d", i))), &high); err != nil {
				return err
			}
			if !high.Quarantined || high.Revision != 2 {
				return fmt.Errorf("quarantine lost or repeated: %d", i)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
