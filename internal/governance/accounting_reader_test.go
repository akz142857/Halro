package governance

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

func TestOutcomeManagerUsesDiskWorkUnitActivityIncludingLateSettlement(t *testing.T) {
	m, journal, _, definition, _ := testManager(t)
	defer journal.Close()
	ctx := context.Background()
	key := make([]byte, 32)
	key[0] = 19
	path := filepath.Join(t.TempDir(), "ledger.wal")
	native, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	now := m.now().Add(-time.Minute)
	event := func(id string, kind ledger.EventKind) ledger.Event {
		return ledger.Event{EventID: id, Kind: kind, ProjectID: "prj_1", KeyID: "key_1", WorkUnitID: "wku_unit", PeriodID: "2026-09-04", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, PeriodStartMicros: now.Truncate(24 * time.Hour).UnixMicro(), PeriodEndMicros: now.Truncate(24 * time.Hour).Add(24 * time.Hour).UnixMicro(), OccurredAt: now,
			Operation: "test." + id, IdempotencyKeyHash: shaLabel(id), RequestFingerprint: shaLabel(id + "-fp")}
	}
	wu := event("evt_wu", ledger.EventWorkUnitCreated)
	wu.OutcomeDefinitions = []domain.OutcomeDefinitionRef{{ID: definition.ID, Version: 1}}
	run := event("evt_run", ledger.EventRunCreated)
	run.RunID, run.RunExpiresAt, run.RunBudgetMicrosUSD = "run_activity", now.Add(time.Hour), 1000
	accepted := event("evt_accepted", ledger.EventRequestAccepted)
	accepted.RequestID, accepted.RunID = "req_activity", run.RunID
	closed := event("evt_close", ledger.EventWorkUnitClosed)
	for _, e := range []ledger.Event{wu, run, accepted, closed} {
		if _, _, err := native.AppendRecordWithReceipt(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	report, partial, err := ledger.InspectReplayAuthenticated(path, key, nil)
	if err != nil || partial {
		t.Fatal(report, partial, err)
	}
	p, err := ledger.BuildLedgerProjection(ctx, path, key, filepath.Join(t.TempDir(), "view.db"), report)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	m.accounting = p // No Ledger State participates in these Outcome reads.
	input := reportInput("accepted", "", "disk-activity")
	first, replay, err := m.Report(ctx, input)
	if err != nil || replay || !first.Provisional {
		t.Fatal("accepted pre-reservation request lost provisional status", first, replay, err)
	}
	appendApply := func(e ledger.Event) {
		t.Helper()
		if _, _, err := native.AppendApplyRecordWithReceipt(ctx, e, p.Apply); err != nil {
			t.Fatal(err)
		}
	}
	reserve := accepted
	reserve.EventID, reserve.Kind, reserve.AttemptID, reserve.ReservationMicrosUSD = "evt_reserve", ledger.EventReservationCreated, "att_activity", ledger.MicrosUSD(7)
	appendApply(reserve)
	finalized := accepted
	finalized.EventID, finalized.Kind = "evt_final", ledger.EventRequestFinalized
	appendApply(finalized)
	again, replay, err := m.Report(ctx, input)
	if err != nil || !replay || again.ID != first.ID || !again.Provisional || journal.Summary().Records != 1 {
		t.Fatal("finalized pending attempt became final or duplicated Outcome", again, replay, err)
	}
	settled := reserve
	settled.EventID, settled.Kind, settled.CommittedMicrosUSD, settled.Outcome = "evt_settle", ledger.EventAttemptSettled, ledger.MicrosUSD(3), "success"
	appendApply(settled)
	again, replay, err = m.Report(ctx, input)
	if err != nil || !replay || again.Provisional || journal.Summary().Records != 1 {
		t.Fatal("late settlement did not update replay status", again, replay, err)
	}
	items, err := m.OutcomesContext(ctx, "prj_1", "wku_unit", "")
	if err != nil || len(items) != 1 || items[0].Provisional {
		t.Fatal(items, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Report(ctx, input); !errors.Is(err, ErrUnavailable) || !errors.Is(err, ledger.ErrProjectionUnavailable) {
		t.Fatal("unavailable view credited an idempotent answer", err)
	}
}

type failingOutcomeAccounting struct {
	ledger.AccountingWorkUnitReader
	calls, failAt int
	err           error
}

func (r *failingOutcomeAccounting) ReadWorkUnitAccounting(ctx context.Context, project, unit string, runs bool) (ledger.WorkUnitAccountingView, bool, error) {
	r.calls++
	v, found, err := r.AccountingWorkUnitReader.ReadWorkUnitAccounting(ctx, project, unit, runs)
	if r.failAt == 0 || r.calls == r.failAt {
		return v, found, errors.Join(err, r.err)
	}
	return v, found, err
}

func TestOutcomeAccountingReadFailureBeforeAndAfterCommitPreservesRetry(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(map[int]string{1: "pre_append", 2: "post_commit"}[failAt], func(t *testing.T) {
			m, journal, state, _, _ := testManager(t)
			defer journal.Close()
			injected := errors.New("controlled accounting view failure")
			reader := &failingOutcomeAccounting{AccountingWorkUnitReader: state, failAt: failAt, err: injected}
			m.accounting = reader
			input := reportInput("accepted", "", "retry")
			outcome, _, err := m.Report(context.Background(), input)
			if !errors.Is(err, ErrUnavailable) || !errors.Is(err, injected) || outcome.ID != "" || journal.Summary().Records != uint64(failAt-1) {
				t.Fatal(outcome, err, journal.Summary())
			}
			reader.failAt = -1
			outcome, replay, err := m.Report(context.Background(), input)
			if err != nil || outcome.ID == "" || !outcome.Provisional || replay != (failAt == 2) || journal.Summary().Records != 1 {
				t.Fatal("retry lost committed Outcome or appended duplicate", outcome, replay, err)
			}
			reader.failAt = 0
			if items, err := m.OutcomesContext(context.Background(), "prj_1", "wku_unit", ""); !errors.Is(err, ErrUnavailable) || len(items) != 0 {
				t.Fatal(items, err)
			}
		})
	}
}
