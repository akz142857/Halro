package budget

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
)

type recoveryTestWriter struct {
	t                *testing.T
	log              *ledger.Log
	p                *ledger.LedgerProjection
	appended         []ledger.Event
	confirmed        ledger.Watermark
	confirmCalls     int
	failConfirm      int
	failKind         ledger.EventKind
	failAfterDurable bool
	lastSettlement   uint64
}

func TestProjectedRecoveryLegacyRealLeaseOriginSurvivesSettlementAndRestart(t *testing.T) {
	for _, shape := range []string{"started", "unstarted", "multi", "settled", "late", "final-before-lease", "lease-then-accept", "final-before-lease-then-accept", "lease-final-accept", "final-accept-lease", "cross-period", "interrupt-finalize"} {
		t.Run(shape, func(t *testing.T) {
			fixtureShape := "started"
			if shape == "unstarted" {
				fixtureShape = "unstarted"
			}
			if shape == "settled" {
				fixtureShape = "settled"
			}
			if shape == "late" || shape == "final-before-lease" || shape == "final-before-lease-then-accept" || shape == "lease-final-accept" || shape == "final-accept-lease" {
				fixtureShape = "late"
			}
			original := projectedRecoveryFixture(t, 1, fixtureShape, false)
			var events []ledger.Event
			for _, e := range original {
				if e.Kind != ledger.EventRequestAccepted {
					events = append(events, e)
				}
			}
			if shape == "multi" || shape == "cross-period" {
				for _, e := range events[:2] {
					e.EventID += "_second"
					e.AttemptID += "_second"
					e.KeyID, e.RequestedModel = "legacy-other-key", "legacy-other-model"
					if shape == "cross-period" {
						e.PeriodID += "_second"
						e.PeriodTimezoneVersion++
					}
					events = append(events, e)
				}
			}
			if shape == "final-before-lease" || shape == "final-before-lease-then-accept" || shape == "final-accept-lease" {
				events = append([]ledger.Event{events[len(events)-1]}, events[:len(events)-1]...)
			}
			if shape == "lease-then-accept" {
				events = append([]ledger.Event{events[0], original[0]}, events[1:]...)
			}
			if shape == "final-before-lease-then-accept" || shape == "lease-final-accept" {
				events = append(events, original[0])
			}
			if shape == "final-accept-lease" {
				events = append([]ledger.Event{events[0], original[0]}, events[1:]...)
			}
			w, path, key := newProjectedRecoveryTest(t, events)
			originalTerminal, _, terminalErr := w.p.ReadFinalization(context.Background(), events[0].RequestID)
			if terminalErr != nil {
				t.Fatal(terminalErr)
			}
			if shape == "interrupt-finalize" {
				w.failKind = ledger.EventRequestFinalized
			}
			result, err := RecoverProjectedRequests(context.Background(), w.p, w, RecoveryStandalone, ledger.ProjectionPageLimits{Rows: 1, Bytes: 8 << 20})
			if shape == "interrupt-finalize" {
				if !errors.Is(err, errRecoveryInjected) || result.AttemptsSettled != 1 || result.RequestsFinalized != 0 {
					t.Fatal(result, err)
				}
				if err := w.p.Close(); err != nil {
					t.Fatal(err)
				}
				w.p = rebuildRecoveryTestProjection(t, path, key, "retry.db")
				w.failKind = 0
				result, err = RecoverProjectedRequests(context.Background(), w.p, w, RecoveryStandalone, ledger.ProjectionPageLimits{Rows: 1, Bytes: 8 << 20})
				if result.AttemptsSettled != 0 || result.RequestsFinalized != 1 {
					t.Fatal("settled-only legacy request lost recovery entry on restart", result, err)
				}
			}
			if err != nil {
				t.Fatal(result, err)
			}
			request, found, err := w.p.ReadRequest(context.Background(), events[0].RequestID)
			if err != nil || !found || request.LegacyLease == nil || request.Finalized == nil {
				t.Fatal(request, found, err)
			}
			if !strings.Contains(shape, "accept") && request.Accepted.Sequence != 0 {
				t.Fatal("recovery fabricated legacy acceptance", request)
			}
			if shape == "late" || shape == "final-before-lease" || shape == "final-before-lease-then-accept" || shape == "lease-final-accept" || shape == "final-accept-lease" {
				if result.RequestsFinalized != 0 || !reflect.DeepEqual(*request.Finalized, originalTerminal) {
					t.Fatal("legacy terminal fact was replaced", result, request)
				}
			}
			a, err := usage.NewAggregateWithFinalizationReader(w.p)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.log.Replay(ledger.Watermark{}, a.Apply); err != nil {
				t.Fatal(err)
			}
			if a.Metrics().ActiveRequests != 0 || len(a.Snapshot().Requests) != 1 {
				t.Fatal("legacy recovery left activity or duplicated summary", a.Metrics(), a.Snapshot())
			}
			before, _, err := w.p.ReadPosition(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			again, err := RecoverProjectedRequests(context.Background(), w.p, w, RecoveryStandalone, ledger.ProjectionPageLimits{Rows: 1, Bytes: 8 << 20})
			if err != nil || again.AttemptsSettled != 0 || again.RequestsFinalized != 0 || again.ConfirmedHead != before {
				t.Fatal("legacy recovery retry duplicated facts", again, err)
			}
		})
	}

}

var errRecoveryInjected = errors.New("injected recovery failure")

func (w *recoveryTestWriter) AppendApplyRecovery(ctx context.Context, event ledger.Event) (ledger.Record, error) {
	if w.failKind == event.Kind && !w.failAfterDurable {
		return ledger.Record{}, errRecoveryInjected
	}
	if event.Kind == ledger.EventRequestFinalized && w.confirmed.Sequence < w.lastSettlement {
		w.t.Fatal("finalized before settlements were confirmed")
	}
	record, _, err := w.log.AppendRecordWithReceipt(ctx, event)
	if err != nil {
		return ledger.Record{}, err
	}
	if w.failKind == event.Kind && w.failAfterDurable {
		return ledger.Record{}, errRecoveryInjected
	}
	if err := w.p.Apply(ctx, record); err != nil {
		return ledger.Record{}, err
	}
	w.appended = append(w.appended, event)
	if event.Kind == ledger.EventAttemptSettled {
		w.lastSettlement = record.Sequence
	}
	return record, nil
}

func (w *recoveryTestWriter) ConfirmRecoveryPrefix(ctx context.Context, head ledger.Watermark) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.confirmCalls++
	if w.confirmCalls == w.failConfirm {
		return errRecoveryInjected
	}
	actual, actualHash, err := w.p.ReadPosition(ctx)
	if err != nil {
		return err
	}
	nativeHead, nativeHash, authenticated := w.log.ChainHead()
	if !authenticated || actual != head || actual != nativeHead || actualHash != nativeHash {
		return errors.New("confirmation target differs from actual applied/native head or chain hash")
	}
	w.confirmed = head
	return nil
}

func projectedRecoveryFixture(t *testing.T, n int, shape string, attributed bool) []ledger.Event {
	t.Helper()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.FixedZone("fixture", 19800))
	base := ledger.Event{ProjectID: "prj_recovery", RequestID: fmt.Sprint("req_", n), KeyID: "key", RequestedModel: "model", PeriodID: "2026-10-02", PeriodTimezone: "Asia/Kolkata", PeriodTimezoneVersion: 3, PeriodStartMicros: now.Add(-12 * time.Hour).UnixMicro(), PeriodEndMicros: now.Add(12 * time.Hour).UnixMicro(), OccurredAt: now}
	var events []ledger.Event
	if attributed {
		base.WorkUnitID, base.RunID = fmt.Sprint("wku_", n), fmt.Sprint("run_", n)
		wu := base
		wu.EventID = fmt.Sprint("wu_", n)
		wu.Kind = ledger.EventWorkUnitCreated
		wu.RunID = ""
		wu.Operation = wu.EventID
		wu.IdempotencyKeyHash = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		wu.RequestFingerprint = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		run := wu
		run.Kind = ledger.EventRunCreated
		run.EventID = fmt.Sprint("run_", n)
		run.RunID = base.RunID
		run.Operation = run.EventID
		run.RunBudgetMicrosUSD = 10000
		run.RunExpiresAt = now.Add(time.Hour)
		events = append(events, wu, run)
	}
	a := base
	a.Kind = ledger.EventRequestAccepted
	a.EventID = fmt.Sprint("accepted_", n)
	events = append(events, a)
	if shape == "accepted" {
		return events
	}
	r := base
	r.Kind = ledger.EventReservationCreated
	r.EventID = fmt.Sprint("reserved_", n)
	r.AttemptID = fmt.Sprint("att_", n)
	r.ReservationMicrosUSD = ledger.MicrosUSD(50)
	r.LeaseMode = ledger.LeaseModeMetered
	r.PriceSnapshot = testPriceSnapshot(t, domain.BillingModeMetered)
	r.PreparedInputTokens = 10
	r.PreparedOutputTokens = 20
	r.TokenGuardPricingViewDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r.DeploymentID = "dep_test"
	events = append(events, r)
	if shape == "unstarted" {
		return events
	}
	started := r
	started.Kind = ledger.EventAttemptStarted
	started.EventID = fmt.Sprint("started_", n)
	events = append(events, started)
	if shape == "started" {
		return events
	}
	if shape == "late" {
		f := recoveryFinalization(a)
		f.EventID = fmt.Sprint("original_finalized_", n)
		events = append(events, f)
		return events
	}
	attempt, settlement, eventID, err := recoverySettlement(ledger.PendingLease{Reservation: r, Started: true})
	if err != nil {
		t.Fatal(err)
	}
	s, err := buildSettlementEvent(eventID, attempt, settlement, r.OccurredAt)
	if err != nil {
		t.Fatal(err)
	}
	events = append(events, s)
	return events
}

func newProjectedRecoveryTest(t *testing.T, events []ledger.Event) (*recoveryTestWriter, string, []byte) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "ledger.wal")
	key := bytes.Repeat([]byte{0x51}, 32)
	log, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if _, err := log.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	p := rebuildRecoveryTestProjection(t, path, key, "projection.db")
	log, err = ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	w := &recoveryTestWriter{t: t, log: log, p: p}
	t.Cleanup(func() {
		if err := w.log.Close(); err != nil {
			t.Error(err)
		}
		if err := w.p.Close(); err != nil {
			t.Error(err)
		}
	})
	return w, path, key
}

func rebuildRecoveryTestProjection(t *testing.T, path string, key []byte, name string) *ledger.LedgerProjection {
	t.Helper()
	report, partial, err := ledger.InspectReplayAuthenticated(path, key, nil)
	if err != nil || partial {
		t.Fatal(partial, err)
	}
	p, err := ledger.BuildLedgerProjection(context.Background(), path, key, filepath.Join(filepath.Dir(path), name), report)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProjectedRecoveryIndependentSetsPreserveAttributionAndUsage(t *testing.T) {
	for _, attributed := range []bool{false, true} {
		t.Run(fmt.Sprint("run=", attributed), func(t *testing.T) {
			var events []ledger.Event
			for n, shape := range []string{"accepted", "unstarted", "started", "settled", "late"} {
				events = append(events, projectedRecoveryFixture(t, n, shape, attributed)...)
			}
			w, path, key := newProjectedRecoveryTest(t, events)
			original, found, err := w.p.ReadFinalization(context.Background(), "req_4")
			if err != nil || !found {
				t.Fatal(found, err)
			}
			result, err := RecoverProjectedRequests(context.Background(), w.p, w, RecoveryStandalone, ledger.ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			if result.AttemptsSettled != 3 || result.RequestsFinalized != 4 || result.ConfirmedHead != w.confirmed {
				t.Fatal(result)
			}
			if after, found, err := w.p.ReadFinalization(context.Background(), "req_4"); err != nil || !found || !reflect.DeepEqual(original, after) {
				t.Fatal("rewrote original terminal", after, found, err)
			}
			for n := 0; n < 5; n++ {
				r, found, err := w.p.ReadRequest(context.Background(), fmt.Sprint("req_", n))
				if err != nil || !found || r.Finalized == nil {
					t.Fatal(n, found, err)
				}
				if n != 4 && r.Finalized.Event.Outcome != "recovered_interrupted" {
					t.Fatal(r)
				}
				if attributed {
					if run, found, err := w.p.ReadRun(context.Background(), fmt.Sprint("run_", n)); err != nil || !found || run.ReservedMicrosUSD != 0 {
						t.Fatal(run, found, err)
					}
				}
			}
			before := len(w.appended)
			again, err := RecoverProjectedRequests(context.Background(), w.p, w, RecoveryStandalone, ledger.ProjectionPageLimits{Rows: 2, Bytes: 1 << 20})
			if err != nil || again.AttemptsSettled != 0 || again.RequestsFinalized != 0 || len(w.appended) != before {
				t.Fatal(again, err)
			}
			a, err := usage.NewAggregateWithFinalizationReader(w.p)
			if err != nil {
				t.Fatal(err)
			}
			_, partial, err := ledger.InspectReplayAuthenticated(path, key, a.Apply)
			if err != nil || partial {
				t.Fatal(partial, err)
			}
			s := a.Snapshot()
			if a.Metrics().ActiveRequests != 0 || len(s.Requests) != 5 || len(s.Attempts) != 4 || s.Totals.Requests != 5 || s.Totals.CostMicrosUSD != 150 {
				t.Fatal(s, a.Metrics())
			}
		})
	}
}

func TestProjectedRecoveryConfirmationFailureStopsBeforeRequestFinalization(t *testing.T) {
	w, _, _ := newProjectedRecoveryTest(t, projectedRecoveryFixture(t, 1, "started", true))
	w.failConfirm = 2
	result, err := RecoverProjectedRequests(context.Background(), w.p, w, RecoveryPrimary, ledger.ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
	if !errors.Is(err, errRecoveryInjected) || result.AttemptsSettled != 1 || result.RequestsFinalized != 0 {
		t.Fatal(result, err)
	}
	r, _, err := w.p.ReadRequest(context.Background(), "req_1")
	if err != nil || r.Finalized != nil {
		t.Fatal(r, err)
	}
	w.failConfirm = 0
	result, err = RecoverProjectedRequests(context.Background(), w.p, w, RecoveryPrimary, ledger.ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
	if err != nil || result.AttemptsSettled != 0 || result.RequestsFinalized != 1 {
		t.Fatal(result, err)
	}
}

func TestProjectedRecoveryDurableTerminalRebuildDoesNotDuplicateSummary(t *testing.T) {
	w, path, key := newProjectedRecoveryTest(t, projectedRecoveryFixture(t, 1, "accepted", true))
	w.failKind = ledger.EventRequestFinalized
	w.failAfterDurable = true
	_, err := RecoverProjectedRequests(context.Background(), w.p, w, RecoveryStandalone, ledger.ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
	if !errors.Is(err, errRecoveryInjected) {
		t.Fatal(err)
	}
	if err := w.log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.p.Close(); err != nil {
		t.Fatal(err)
	}
	w.p = rebuildRecoveryTestProjection(t, path, key, "reconstructed.db")
	w.log, err = ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	w.failKind = 0
	w.failAfterDurable = false
	result, err := RecoverProjectedRequests(context.Background(), w.p, w, RecoveryStandalone, ledger.ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
	if err != nil || result.RequestsFinalized != 0 || len(w.appended) != 0 {
		t.Fatal(result, err)
	}
	a, err := usage.NewAggregateWithFinalizationReader(w.p)
	if err != nil {
		t.Fatal(err)
	}
	_, partial, err := ledger.InspectReplayAuthenticated(path, key, a.Apply)
	if err != nil || partial || len(a.Snapshot().Requests) != 1 || a.Metrics().ActiveRequests != 0 {
		t.Fatal(partial, err, a.Snapshot())
	}
}

func TestProjectedRecoveryRejectsRoleLimitsAndUnconfirmedInputWithoutWrites(t *testing.T) {
	w, _, _ := newProjectedRecoveryTest(t, projectedRecoveryFixture(t, 1, "accepted", false))
	limits := ledger.ProjectionPageLimits{Rows: 1, Bytes: 1 << 20}
	for _, role := range []RecoveryRole{"replica", "unknown", ""} {
		if _, err := RecoverProjectedRequests(context.Background(), w.p, w, role, limits); err == nil {
			t.Fatal("write role accepted", role)
		}
	}
	if _, err := RecoverProjectedRequests(context.Background(), w.p, w, RecoveryStandalone, ledger.ProjectionPageLimits{Rows: 65, Bytes: 1 << 20}); err == nil {
		t.Fatal("unbounded recovery page accepted")
	}
	w.failConfirm = 1
	if _, err := RecoverProjectedRequests(context.Background(), w.p, w, RecoveryPrimary, limits); !errors.Is(err, errRecoveryInjected) || len(w.appended) != 0 {
		t.Fatal(err, w.appended)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RecoverProjectedRequests(ctx, w.p, w, RecoveryStandalone, limits); !errors.Is(err, context.Canceled) || len(w.appended) != 0 {
		t.Fatal(err, w.appended)
	}
}

func TestProjectedRecoveryDurableSettlementRebuildDoesNotSettleTwice(t *testing.T) {
	w, path, key := newProjectedRecoveryTest(t, projectedRecoveryFixture(t, 1, "started", true))
	w.failKind, w.failAfterDurable = ledger.EventAttemptSettled, true
	_, err := RecoverProjectedRequests(context.Background(), w.p, w, RecoveryStandalone, ledger.ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
	if !errors.Is(err, errRecoveryInjected) {
		t.Fatal(err)
	}
	if err := w.log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.p.Close(); err != nil {
		t.Fatal(err)
	}
	w.p = rebuildRecoveryTestProjection(t, path, key, "reconstructed.db")
	w.log, err = ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	w.failKind, w.failAfterDurable = 0, false
	result, err := RecoverProjectedRequests(context.Background(), w.p, w, RecoveryStandalone, ledger.ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
	if err != nil || result.AttemptsSettled != 0 || result.RequestsFinalized != 1 {
		t.Fatal(result, err)
	}
	balance, err := w.p.ReadBalance(context.Background(), ledger.BalanceKey{ProjectID: "prj_recovery", PeriodID: "2026-10-02", TimezoneVersion: 3})
	if err != nil || balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD != 50 {
		t.Fatal(balance, err)
	}
	a, err := usage.NewAggregateWithFinalizationReader(w.p)
	if err != nil {
		t.Fatal(err)
	}
	_, partial, err := ledger.InspectReplayAuthenticated(path, key, a.Apply)
	if err != nil || partial || len(a.Snapshot().Attempts) != 1 || len(a.Snapshot().Requests) != 1 {
		t.Fatal(partial, err, a.Snapshot())
	}
}

func TestProjectedRecoveryTerminalConfirmationFailureRetriesEmptySetWithConfirmation(t *testing.T) {
	w, _, _ := newProjectedRecoveryTest(t, projectedRecoveryFixture(t, 1, "accepted", true))
	w.failConfirm = 2
	result, err := RecoverProjectedRequests(context.Background(), w.p, w, RecoveryPrimary, ledger.ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
	if !errors.Is(err, errRecoveryInjected) || result.RequestsFinalized != 1 || result.ConfirmedHead != result.InputHead {
		t.Fatal(result, err)
	}
	original, found, err := w.p.ReadFinalization(context.Background(), "req_1")
	if err != nil || !found {
		t.Fatal(found, err)
	}
	count, calls := len(w.appended), w.confirmCalls
	w.failConfirm = 0
	result, err = RecoverProjectedRequests(context.Background(), w.p, w, RecoveryPrimary, ledger.ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
	if err != nil || result.RequestsFinalized != 0 || len(w.appended) != count || w.confirmCalls != calls+2 || result.ConfirmedHead.Sequence != original.Sequence {
		t.Fatal(result, err, w.confirmCalls)
	}
}

func TestProjectedRecoveryMultipleAttemptsAndLegacyBounds(t *testing.T) {
	events := projectedRecoveryFixture(t, 1, "settled", true)
	reservation := events[3]
	for n := 0; n < 2; n++ {
		r := reservation
		r.AttemptID = fmt.Sprint("extra_", n)
		r.EventID = fmt.Sprint("extra_reserved_", n)
		events = append(events, r)
		if n == 0 {
			started := r
			started.Kind = ledger.EventAttemptStarted
			started.EventID = "extra_started"
			events = append(events, started)
		}
	}
	// Exercise absent legacy period bounds on all facts while retaining frozen
	// period/timezone identity. Recovery must preserve zero, not synthesize it.
	for n := range events {
		events[n].PeriodStartMicros = 0
		events[n].PeriodEndMicros = 0
	}
	w, _, _ := newProjectedRecoveryTest(t, events)
	result, err := RecoverProjectedRequests(context.Background(), w.p, w, RecoveryStandalone, ledger.ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
	if err != nil || result.AttemptsSettled != 2 || result.RequestsFinalized != 1 {
		t.Fatal(result, err)
	}
	for _, event := range w.appended {
		if event.PeriodStartMicros != 0 || event.PeriodEndMicros != 0 {
			t.Fatal("changed legacy bounds", event)
		}
	}
	balance, err := w.p.ReadBalance(context.Background(), ledger.BalanceKey{ProjectID: "prj_recovery", PeriodID: "2026-10-02", TimezoneVersion: 3})
	if err != nil || balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD != 100 {
		t.Fatal(balance, err)
	}
}
