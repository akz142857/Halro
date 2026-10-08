package ledger

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func TestAccountingAdmissionReadersMatchAndOwnReturnedValues(t *testing.T) {
	p := projectionTestStore(t)
	state := NewState()
	ctx := context.Background()
	events := projectionTestAttribution()
	events[0].OutcomeDefinitions = []domain.OutcomeDefinitionRef{{ID: "odef_result", Version: 1}}
	closeRun := governanceEvent("evt_close_run", EventRunClosed, events[0].OccurredAt.Add(time.Minute))
	closeRun.WorkUnitID, closeRun.RunID, closeRun.CloseReason = "wku_1", "run_1", "operator"
	closeUnit := governanceEvent("evt_close_unit", EventWorkUnitClosed, events[0].OccurredAt.Add(2*time.Minute))
	closeUnit.WorkUnitID = "wku_1"
	events = append(events, closeRun, closeUnit)
	for i, event := range events {
		r := projectionTestRecord(uint64(i+1), event)
		if err := state.Apply(r); err != nil {
			t.Fatal(err)
		}
		if err := p.Apply(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	wantUnit, _, _ := state.ReadWorkUnit(ctx, "wku_1")
	wantRun, _, _ := state.ReadRun(ctx, "run_1")
	for _, reader := range []AccountingAdmissionReader{state, p} {
		unit, found, err := reader.ReadWorkUnit(ctx, "wku_1")
		if err != nil || !found || !reflect.DeepEqual(unit, wantUnit) {
			t.Fatal(unit, found, err)
		}
		run, found, err := reader.ReadRun(ctx, "run_1")
		if err != nil || !found || !reflect.DeepEqual(run, wantRun) {
			t.Fatal(run, found, err)
		}
		unit.OutcomeDefinitions[0].ID = "odef_changed"
		*unit.ClosedAt = time.Time{}
		*run.ClosedAt = time.Time{}
		againUnit, _, err := reader.ReadWorkUnit(ctx, "wku_1")
		if err != nil || !reflect.DeepEqual(againUnit, wantUnit) {
			t.Fatal("caller mutated owned work-unit evidence", againUnit, err)
		}
		againRun, _, err := reader.ReadRun(ctx, "run_1")
		if err != nil || !reflect.DeepEqual(againRun, wantRun) {
			t.Fatal("caller mutated owned run evidence", againRun, err)
		}
		for _, event := range events {
			want, _, _ := state.ReadGovernanceIdempotency(ctx, event.ProjectID, event.Operation, event.IdempotencyKeyHash)
			got, found, err := reader.ReadGovernanceIdempotency(ctx, event.ProjectID, event.Operation, event.IdempotencyKeyHash)
			if err != nil || !found || got != want {
				t.Fatal(got, want, found, err)
			}
		}
		if _, found, err := reader.ReadRun(ctx, "run_absent"); err != nil || found {
			t.Fatal("absence is not a successful absent read", found, err)
		}
		if got, err := reader.ReadBalance(ctx, BalanceKey{ProjectID: "prj_absent"}); err != nil || got != (Balance{}) {
			t.Fatal("initial balance", got, err)
		}
	}
}

func TestAccountingAdmissionProjectionRejectsMismatchedAndMalformedRows(t *testing.T) {
	for _, kind := range []string{"run_identity", "run_bounds", "work_unit_identity", "work_unit_status", "idempotency_identity", "balance_negative", "balance_json"} {
		t.Run(kind, func(t *testing.T) {
			p := projectionTestStore(t)
			ctx := context.Background()
			events := projectionTestAttribution()
			projectionApplyEvents(t, p, events, 0)
			unit, _, _ := p.ReadWorkUnit(ctx, "wku_1")
			run, _, _ := p.ReadRun(ctx, "run_1")
			idem, _, _ := p.ReadGovernanceIdempotency(ctx, events[0].ProjectID, events[0].Operation, events[0].IdempotencyKeyHash)
			key := BalanceKey{ProjectID: "prj_1", PeriodID: "2026-10-02", TimezoneVersion: 1}
			if err := p.db.Update(func(tx *bbolt.Tx) error {
				switch kind {
				case "run_identity":
					run.ID = "run_other"
					return projectionPut(tx, "runs", projectionIDKey("run_1"), run)
				case "run_bounds":
					run.ReservedMicrosUSD = -1
					return projectionPut(tx, "runs", projectionIDKey("run_1"), run)
				case "work_unit_identity":
					unit.ID = "wku_other"
					return projectionPut(tx, "work_units", projectionIDKey("wku_1"), unit)
				case "work_unit_status":
					unit.Status = "invalid"
					return projectionPut(tx, "work_units", projectionIDKey("wku_1"), unit)
				case "idempotency_identity":
					idem.ProjectID = "prj_other"
					return projectionPut(tx, "idempotency", projectionTuple(events[0].ProjectID, events[0].Operation, events[0].IdempotencyKeyHash), idem)
				case "balance_negative":
					return projectionPut(tx, "balances", projectionBalanceKey(key), Balance{CommittedMicrosUSD: -1})
				default:
					return tx.Bucket([]byte("balances")).Put(projectionBalanceKey(key), []byte("{broken"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "run_identity", "run_bounds":
				var got domain.Run
				var found bool
				got, found, err = p.ReadRun(ctx, "run_1")
				if found || !reflect.DeepEqual(got, domain.Run{}) {
					t.Fatal("failed read returned an authoritative value")
				}
			case "work_unit_identity", "work_unit_status":
				_, _, err = p.ReadWorkUnit(ctx, "wku_1")
			case "idempotency_identity":
				_, _, err = p.ReadGovernanceIdempotency(ctx, events[0].ProjectID, events[0].Operation, events[0].IdempotencyKeyHash)
			default:
				got, readErr := p.ReadBalance(ctx, key)
				err = readErr
				if got != (Balance{}) {
					t.Fatal("failed read returned a financial value", got)
				}
			}
			if !errors.Is(err, ErrProjectionCorrupt) {
				t.Fatal("bad row was treated as absence", err)
			}
		})
	}
}

func TestAccountingAdmissionReadersRespectCancellationAndLifetime(t *testing.T) {
	p := projectionTestStore(t)
	state := NewState()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	check := func(reader AccountingAdmissionReader, ctx context.Context, want error) {
		t.Helper()
		if _, err := reader.ReadBalance(ctx, BalanceKey{}); err == nil || (want != nil && !errors.Is(err, want)) {
			t.Fatal(err)
		}
		if _, found, err := reader.ReadRun(ctx, "run_absent"); found || err == nil || (want != nil && !errors.Is(err, want)) {
			t.Fatal(found, err)
		}
		if _, found, err := reader.ReadWorkUnit(ctx, "wku_absent"); found || err == nil || (want != nil && !errors.Is(err, want)) {
			t.Fatal(found, err)
		}
		if _, found, err := reader.ReadGovernanceIdempotency(ctx, "prj_1", "create", "hash"); found || err == nil || (want != nil && !errors.Is(err, want)) {
			t.Fatal(found, err)
		}
	}
	for _, reader := range []AccountingAdmissionReader{state, p} {
		check(reader, canceled, context.Canceled)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	check(p, context.Background(), ErrProjectionUnavailable)
	check((*LedgerProjection)(nil), context.Background(), ErrProjectionUnavailable)
	check((*State)(nil), context.Background(), nil)
}
