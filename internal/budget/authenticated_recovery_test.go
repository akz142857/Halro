package budget

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type authenticatedRecoveryTestWriter struct {
	t                *testing.T
	native           *ledger.Log
	p                *ledger.AuthenticatedLedgerProjection
	lease            *ledger.MaintenanceLease
	confirmed        ledger.Watermark
	confirmCalls     int
	failConfirm      int
	failKind         ledger.EventKind
	failAfterDurable bool
	lastSettlement   uint64
}

func (w *authenticatedRecoveryTestWriter) AppendApplyRecovery(ctx context.Context, event ledger.Event) (ledger.Record, error) {
	if w.failKind == event.Kind && !w.failAfterDurable {
		return ledger.Record{}, errRecoveryInjected
	}
	if event.Kind == ledger.EventRequestFinalized && w.confirmed.Sequence < w.lastSettlement {
		w.t.Fatal("finalization preceded full settlement-batch confirmation")
	}
	r, _, err := w.lease.AppendApply(ctx, event, func(ctx context.Context, r ledger.Record) error {
		if w.failKind == event.Kind && w.failAfterDurable {
			return errRecoveryInjected
		}
		_, err := w.p.AdvanceOneFromNative(ctx, w.native, ledger.ProjectionPrefix{Head: ledger.Watermark{Generation: r.Generation, Offset: r.Offset, Sequence: r.Sequence}, Hash: r.Hash})
		return err
	})
	if err == nil && event.Kind == ledger.EventAttemptSettled {
		w.lastSettlement = r.Sequence
	}
	return r, err
}

func (w *authenticatedRecoveryTestWriter) ConfirmRecoveryPrefix(ctx context.Context, expected ledger.Watermark) error {
	w.confirmCalls++
	if w.confirmCalls == w.failConfirm {
		return errRecoveryInjected
	}
	err := w.lease.WithHead(ctx, func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		actual, actualHash, err := w.p.ReadPosition(ctx)
		if err != nil || actual != head || actualHash != hash || head != expected {
			return errors.Join(err, errors.New("confirmation did not bind actual complete native cut"))
		}
		return nil
	})
	if err == nil {
		w.confirmed = expected
	}
	return err
}

func authenticatedRecoveryTestCandidate(t *testing.T, native *ledger.Log, path string, key []byte) *authenticatedRecoveryTestWriter {
	t.Helper()
	ctx := context.Background()
	lease, err := native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 256 << 20, Disk: 256 << 20}, MaxClaims: 16, MaxOwners: 4, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 16 * resourcebudget.ClaimRAMBytes}
	resources, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := resources.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	var p *ledger.AuthenticatedLedgerProjection
	err = lease.WithHead(ctx, func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		report, partial, err := ledger.InspectReplayAuthenticated(path, key, nil)
		if err != nil || partial || report.Head != head || report.ChainHash != hash {
			return errors.Join(err, errors.New("fixture native cut mismatch"))
		}
		p, err = ledger.BuildAuthenticatedLedgerProjection(ctx, path, key, filepath.Join(t.TempDir(), "auth"), report, ledger.AuthenticatedLedgerOptions{Identity: sha256.Sum256([]byte("independent-recovery-test")), Budget: resources, MaxDiskBytes: 128 << 20, MaxAppendBytes: 8 << 20})
		return err
	})
	if err != nil {
		lease.Close()
		t.Fatal(err)
	}
	if err := p.ValidateNativeWriter(ctx, native, lease); err != nil {
		t.Fatal(err)
	}
	w := &authenticatedRecoveryTestWriter{t: t, native: native, p: p, lease: lease}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
		if err := native.Close(); err != nil {
			t.Error(err)
		}
		if err := p.Discard(); err != nil {
			t.Error(err)
		}
		if used := resources.Snapshot().Used; used != (resourcebudget.Cost{}) {
			t.Error("recovery leaked resource owner", used)
		}
	})
	return w
}

func TestAuthenticatedTwoLevelRecoveryIndependentInputsAndRetry(t *testing.T) {
	for _, attributed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unattributed", true: "attributed"}[attributed], func(t *testing.T) {
			var events []ledger.Event
			for i, shape := range []string{"accepted", "unstarted", "started", "settled", "late"} {
				events = append(events, projectedRecoveryFixture(t, i+1, shape, attributed)...)
			}
			// The third request has a second independently indexed pending attempt.
			for _, e := range projectedRecoveryFixture(t, 3, "started", attributed) {
				if e.Kind == ledger.EventReservationCreated || e.Kind == ledger.EventAttemptStarted {
					e.AttemptID += "_second"
					e.EventID += "_second"
					events = append(events, e)
				}
			}
			base, path, key := newProjectedRecoveryTest(t, events)
			w := authenticatedRecoveryTestCandidate(t, base.log, path, key)
			ctx := context.Background()
			limits := ledger.ProjectionPageLimits{Rows: 1, Bytes: 1 << 20}
			late, found, err := w.p.ReadRequest(ctx, "req_5")
			if err != nil || !found || late.Finalized == nil {
				t.Fatal(late, found, err)
			}
			result, err := RecoverAuthenticatedRequests(ctx, w.p, w, RecoveryStandalone, limits)
			if err != nil || result.AttemptsSettled != 4 || result.RequestsFinalized != 4 || result.InputHead.Sequence != uint64(len(events)) || result.ConfirmedHead.Sequence != uint64(len(events))+8 {
				t.Fatal(result, err)
			}
			actual, _, err := w.p.ReadRequest(ctx, "req_5")
			if err != nil || !reflect.DeepEqual(actual.Finalized, late.Finalized) {
				t.Fatal("late settlement rewrote original terminal", actual, late, err)
			}
			for _, id := range []string{"att_2", "att_3", "att_3_second", "att_4", "att_5"} {
				s, found, err := w.p.ReadSettlement(ctx, id)
				want := int64(50)
				if id == "att_2" {
					want = 0
				}
				if err != nil || !found || s.CostMicrosUSD != want {
					t.Fatal("conservative settlement changed", id, s, found, err)
				}
			}
			stats, err := w.p.ReadPendingLeaseStats(ctx, time.Now())
			if err != nil || stats.Count != 0 {
				t.Fatal(stats, err)
			}
			calls := w.confirmCalls
			again, err := RecoverAuthenticatedRequests(ctx, w.p, w, RecoveryStandalone, limits)
			if err != nil || again.AttemptsSettled != 0 || again.RequestsFinalized != 0 || again.ConfirmedHead != result.ConfirmedHead || w.confirmCalls != calls+2 {
				t.Fatal("empty retry skipped confirmation or duplicated facts", again, err)
			}
		})
	}
}

func TestAuthenticatedTwoLevelRecoveryConfirmationFailureAndFreshDurableRetry(t *testing.T) {
	for _, phase := range []string{"confirm-settlements", "durable-settlement", "durable-finalization", "confirm-finalization"} {
		t.Run(phase, func(t *testing.T) {
			events := projectedRecoveryFixture(t, 1, "started", true)
			if phase == "durable-finalization" || phase == "confirm-finalization" {
				events = projectedRecoveryFixture(t, 1, "accepted", true)
			}
			base, path, key := newProjectedRecoveryTest(t, events)
			w := authenticatedRecoveryTestCandidate(t, base.log, path, key)
			switch phase {
			case "confirm-settlements", "confirm-finalization":
				w.failConfirm = 2
			case "durable-settlement":
				w.failKind = ledger.EventAttemptSettled
				w.failAfterDurable = true
			case "durable-finalization":
				w.failKind = ledger.EventRequestFinalized
				w.failAfterDurable = true
			}
			limits := ledger.ProjectionPageLimits{Rows: 1, Bytes: 1 << 20}
			first, err := RecoverAuthenticatedRequests(context.Background(), w.p, w, RecoveryStandalone, limits)
			if !errors.Is(err, errRecoveryInjected) {
				t.Fatal(first, err)
			}
			if phase == "confirm-settlements" && first.RequestsFinalized != 0 {
				t.Fatal("request terminal crossed failed settlement confirmation", first)
			}
			w.lease.Close()
			base.log.Close()
			w.p.Discard()
			reopened, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
			if err != nil {
				t.Fatal(err)
			}
			fresh := authenticatedRecoveryTestCandidate(t, reopened, path, key)
			result, err := RecoverAuthenticatedRequests(context.Background(), fresh.p, fresh, RecoveryStandalone, limits)
			if err != nil || result.AttemptsSettled != 0 || result.RequestsFinalized > 1 {
				t.Fatal("fresh retry repeated durable settlement", result, err)
			}
			if (phase == "durable-finalization" || phase == "confirm-finalization") && result.RequestsFinalized != 0 {
				t.Fatal("fresh retry repeated durable request terminal", result)
			}
			stats, err := fresh.p.ReadPendingLeaseStats(context.Background(), time.Now())
			if err != nil || stats.Count != 0 {
				t.Fatal(stats, err)
			}
		})
	}
}

func TestAuthenticatedTwoLevelRecoveryLegacyOriginAndEmptyInputs(t *testing.T) {
	for _, shape := range []string{"empty", "lease-first", "final-before-lease"} {
		t.Run(shape, func(t *testing.T) {
			var events []ledger.Event
			if shape != "empty" {
				original := projectedRecoveryFixture(t, 1, "late", false)
				for _, e := range original {
					if e.Kind != ledger.EventRequestAccepted {
						events = append(events, e)
					}
				}
				if shape == "lease-first" {
					events = events[:len(events)-1]
				} else {
					events = append([]ledger.Event{events[len(events)-1]}, events[:len(events)-1]...)
				}
			}
			base, path, key := newProjectedRecoveryTest(t, events)
			w := authenticatedRecoveryTestCandidate(t, base.log, path, key)
			result, err := RecoverAuthenticatedRequests(context.Background(), w.p, w, RecoveryStandalone, ledger.ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
			if err != nil {
				t.Fatal(result, err)
			}
			if shape == "empty" {
				if result.AttemptsSettled != 0 || result.RequestsFinalized != 0 || w.confirmCalls != 2 {
					t.Fatal(result)
				}
				return
			}
			parent, found, err := w.p.ReadRequest(context.Background(), "req_1")
			if err != nil || !found || parent.Accepted.Sequence != 0 || parent.LegacyLease == nil || parent.Finalized == nil || result.AttemptsSettled != 1 {
				t.Fatal("legacy recovery fabricated acceptance/lost origin", parent, result, err)
			}
			if shape == "final-before-lease" && (result.RequestsFinalized != 0 || parent.Finalized.Sequence != 1) {
				t.Fatal("original legacy terminal replaced", parent, result)
			}
		})
	}
}
