package usage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

type finalizationReaderFunc func(context.Context, string) (ledger.Record, bool, error)

func (f finalizationReaderFunc) ReadFinalization(ctx context.Context, id string) (ledger.Record, bool, error) {
	return f(ctx, id)
}

type testFinalizationReader struct {
	finalizationReaderFunc
	positions []ledger.Record
}

type eventPositionReader struct {
	FinalizationReader
	read func(context.Context, string) (ledger.Watermark, bool, error)
}

func TestFinalizationReaderRejectsLegacyCheckpointPhantomActivity(t *testing.T) {
	rows := finalizationFixture(true, false)
	old := NewAggregate()
	for _, row := range rows {
		if err := old.Apply(row); err != nil {
			t.Fatal(err)
		}
	}
	if old.Metrics().ActiveRequests != 1 {
		t.Fatal("fixture did not reproduce legacy late-settlement activity")
	}
	store := newCheckpointStore()
	store.round(t, old)
	if a, err := RestoreCheckpointWithFinalizationReader(store.head, store.read, fixtureFinalReader(rows)); err == nil || a != nil {
		t.Fatal("legacy phantom activity was imported", a, err)
	}
}

func TestFinalizationReaderCheckpointRejectsOldDuplicateTerminalTotalsWithoutActiveRows(t *testing.T) {
	rows := finalizationFixture(false, false)
	second := rows[len(rows)-1]
	second.Sequence++
	second.Offset += 100
	second.Event.EventID += "_duplicate_terminal"
	rows = append(rows, second)
	first := rows[len(rows)-2]
	reader := testFinalizationReader{finalizationReaderFunc(func(context.Context, string) (ledger.Record, bool, error) { return first, true, nil }), rows}
	old := NewAggregate()
	for _, r := range rows {
		if err := old.Apply(r); err != nil {
			t.Fatal(err)
		}
	}
	if old.Metrics().ActiveRequests != 0 || len(old.Snapshot().Requests) != 2 {
		t.Fatal("old checkpoint fixture did not count duplicate terminals", old.Snapshot())
	}
	store := newCheckpointStore()
	store.round(t, old)
	if a, err := RestoreCheckpointWithFinalizationReader(store.head, store.read, reader); err == nil || a != nil {
		t.Fatal("legacy totals imported into first-terminal reducer", a, err)
	}
	cold, err := NewAggregateWithFinalizationReader(reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if err := cold.Apply(r); err != nil {
			t.Fatal(err)
		}
	}
	if cold.Metrics().ActiveRequests != 0 || len(cold.Snapshot().Requests) != 1 {
		t.Fatal("first-terminal cold replay counted twice", cold.Snapshot())
	}
	current := newCheckpointStore()
	current.round(t, cold)
	warm, err := RestoreCheckpointWithFinalizationReader(current.head, current.read, reader)
	if err != nil || !reflect.DeepEqual(cold.Snapshot(), warm.Snapshot()) || cold.Metrics() != warm.Metrics() {
		t.Fatal("new checkpoint differs from same-reader cold replay", err)
	}
	if plain, err := RestoreCheckpoint(current.head, current.read); err == nil || plain != nil {
		t.Fatal("first-fact checkpoint resumed under plain semantics", plain, err)
	}
}

func (r eventPositionReader) ReadEventPosition(ctx context.Context, id string) (ledger.Watermark, bool, error) {
	return r.read(ctx, id)
}

func TestFinalizationReaderEventPositionErrorsDoNotMutateUsage(t *testing.T) {
	rows := finalizationFixture(true, false)
	lookupErr := errors.New("event index IO failed")
	for _, reason := range []string{"io", "context", "absent", "zero-sequence", "zero-generation", "zero-offset", "future", "physical-mismatch", "unprocessed-original"} {
		t.Run(reason, func(t *testing.T) {
			base := fixtureFinalReader(rows)
			reader := eventPositionReader{base, func(ctx context.Context, id string) (ledger.Watermark, bool, error) {
				first, found, err := base.ReadEventPosition(ctx, id)
				if id != rows[4].Event.EventID {
					return first, found, err
				}
				switch reason {
				case "io":
					return ledger.Watermark{}, false, lookupErr
				case "context":
					return ledger.Watermark{}, false, context.Canceled
				case "absent":
					return ledger.Watermark{}, false, nil
				case "zero-sequence":
					first.Sequence = 0
				case "zero-generation":
					first.Generation = 0
				case "zero-offset":
					first.Offset = 0
				case "future":
					first.Sequence++
				case "physical-mismatch":
					first.Offset++
				case "unprocessed-original":
					// Consumer head is 4; original is 5, duplicate is 6.
				}
				return first, true, nil
			}}
			a, err := NewAggregateWithFinalizationReader(reader)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows[:4] {
				if err := a.Apply(row); err != nil {
					t.Fatal(err)
				}
			}
			before, metrics := a.Snapshot(), a.Metrics()
			row := rows[4]
			if reason == "unprocessed-original" {
				row.Sequence++
				row.Offset += 100
			}
			err = a.Apply(row)
			if err == nil || (reason == "io" && !errors.Is(err, lookupErr)) {
				t.Fatal("inconsistent event index accepted", err)
			}
			if !reflect.DeepEqual(before, a.Snapshot()) || metrics != a.Metrics() {
				t.Fatal("event lookup failure mutated usage")
			}
		})
	}
}

func (r testFinalizationReader) ReadEventPosition(ctx context.Context, id string) (ledger.Watermark, bool, error) {
	if err := ctx.Err(); err != nil {
		return ledger.Watermark{}, false, err
	}
	for _, record := range r.positions {
		if record.Event.EventID == id {
			return ledger.Watermark{Generation: record.Generation, Sequence: record.Sequence, Offset: record.Offset}, true, nil
		}
	}
	return ledger.Watermark{}, false, nil
}

func finalizationFixture(late, attributed bool) []ledger.Record {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	base := ledger.Event{RequestID: "req", ProjectID: "p", KeyID: "key", RequestedModel: "model", PeriodID: "2026-10-02", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: now}
	if attributed {
		base.WorkUnitID = "wku"
		base.RunID = "run"
	}
	kinds := []ledger.EventKind{ledger.EventRequestAccepted, ledger.EventReservationCreated, ledger.EventAttemptStarted, ledger.EventAttemptSettled, ledger.EventRequestFinalized}
	if late {
		kinds[3], kinds[4] = kinds[4], kinds[3]
	}
	var rows []ledger.Record
	for i, kind := range kinds {
		e := base
		e.Kind = kind
		e.EventID = fmt.Sprint("event_", kind)
		e.OccurredAt = now.Add(time.Duration(i) * time.Second)
		if kind >= ledger.EventReservationCreated && kind <= ledger.EventAttemptSettled {
			e.AttemptID = "att"
		}
		if kind == ledger.EventReservationCreated {
			e.ReservationMicrosUSD = ledger.MicrosUSD(100)
		}
		if kind == ledger.EventAttemptSettled {
			e.CommittedMicrosUSD = ledger.MicrosUSD(7)
			e.ProviderInputTokens = 2
			e.ProviderOutputTokens = 3
			e.Outcome = "recovered_started_unknown_result"
			e.FailurePhase = "accounting"
			e.TokenEstimated = true
			e.CostEstimated = true
			e.Ambiguous = true
		}
		if kind == ledger.EventRequestFinalized {
			e.Outcome = "interrupted"
		}
		rows = append(rows, ledger.Record{Generation: 1, Sequence: uint64(i + 1), Offset: int64((i + 1) * 100), Event: e})
	}
	return rows
}
func fixtureFinalReader(rows []ledger.Record) FinalizationReader {
	var final ledger.Record
	for _, r := range rows {
		if r.Event.Kind == ledger.EventRequestFinalized {
			final = r
		}
	}
	return testFinalizationReader{finalizationReaderFunc(func(ctx context.Context, id string) (ledger.Record, bool, error) {
		if err := ctx.Err(); err != nil {
			return ledger.Record{}, false, err
		}
		return final, id == final.Event.RequestID, nil
	}), rows}
}

func TestFinalizationReaderLateSettlementPreservesTerminalSummaryAndNoActiveRequest(t *testing.T) {
	for _, withRun := range []bool{false, true} {
		t.Run(fmt.Sprint("run=", withRun), func(t *testing.T) {
			rows := finalizationFixture(true, withRun)
			a, err := NewAggregateWithFinalizationReader(fixtureFinalReader(rows))
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows[:4] {
				if err := a.Apply(r); err != nil {
					t.Fatal(err)
				}
			}
			terminal := a.Snapshot().Requests
			if err := a.Apply(rows[4]); err != nil {
				t.Fatal(err)
			}
			snap := a.Snapshot()
			metrics := a.Metrics()
			if len(snap.Attempts) != 1 || snap.Totals.Attempts != 1 || snap.Totals.CostMicrosUSD != 7 || snap.Totals.InputTokens != 2 || snap.Totals.OutputTokens != 3 || snap.Totals.Requests != 1 || metrics.ActiveRequests != 0 {
				t.Fatal(snap, metrics)
			}
			if !reflect.DeepEqual(snap.Requests, terminal) {
				t.Fatal("late settlement modified the existing terminal summary")
			}
			// Duplicate physical recovery facts advance the processing cursor without
			// re-counting attempts, summaries, daily rollups or active work.
			before := a.Snapshot()
			duplicate := rows[4]
			duplicate.Sequence++
			duplicate.Offset += 100
			if err := a.Apply(duplicate); err != nil {
				t.Fatal(err)
			}
			after := a.Snapshot()
			before.Watermark = after.Watermark
			if !reflect.DeepEqual(before, after) || a.Metrics().ActiveRequests != 0 || a.Watermark().Sequence != 6 {
				t.Fatal(after, a.Metrics())
			}
			var attempts, requests, cost int64
			for key, delta := range a.rollupDelta {
				if key.Dimension != domain.RollupDimensionTotal {
					continue
				}
				attempts += delta.Attempts
				requests += delta.Requests
				cost += delta.CostMicrosUSD
			}
			if attempts != 1 || requests != 1 || cost != 7 {
				t.Fatal(attempts, requests, cost)
			}
		})
	}
}

func TestFinalizationReaderAheadOfUsageDoesNotMisclassifyNormalReplay(t *testing.T) {
	rows := finalizationFixture(false, true)
	a, err := NewAggregateWithFinalizationReader(fixtureFinalReader(rows))
	if err != nil {
		t.Fatal(err)
	}
	reference := NewAggregate()
	for _, r := range rows {
		if err := a.Apply(r); err != nil {
			t.Fatal(err)
		}
		if err := reference.Apply(r); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(a.Snapshot(), reference.Snapshot()) || a.Metrics() != reference.Metrics() {
		t.Fatal("latest finalized fact suppressed a normal earlier settlement")
	}
	if got := a.Snapshot().Requests; len(got) != 1 || got[0].Attempts != 1 || got[0].CostMicrosUSD != 7 {
		t.Fatal(got)
	}
}

func TestFinalizationReaderCheckpointResumesLateSuffixAtEveryBoundary(t *testing.T) {
	rows := finalizationFixture(true, true)
	expected, err := NewAggregateWithFinalizationReader(fixtureFinalReader(rows))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if err := expected.Apply(r); err != nil {
			t.Fatal(err)
		}
	}
	for boundary := 1; boundary <= len(rows); boundary++ {
		a, err := NewAggregateWithFinalizationReader(fixtureFinalReader(rows))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows[:boundary] {
			if err := a.Apply(r); err != nil {
				t.Fatal(err)
			}
		}
		store := newCheckpointStore()
		store.round(t, a)
		restored, err := RestoreCheckpointWithFinalizationReader(store.head, store.read, fixtureFinalReader(rows))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows[boundary:] {
			if err := restored.Apply(r); err != nil {
				t.Fatalf("boundary %d: %v", boundary, err)
			}
		}
		if !reflect.DeepEqual(restored.Snapshot(), expected.Snapshot()) || restored.Metrics() != expected.Metrics() {
			t.Fatalf("checkpoint boundary %d diverged", boundary)
		}
	}
}

func TestFinalizationReaderLookupAndAttributionErrorsDoNotMutateUsage(t *testing.T) {
	rows := finalizationFixture(true, true)
	lookupErr := errors.New("request index IO failed")
	for _, reason := range []string{"io", "context", "wrong-request", "wrong-project", "wrong-period", "wrong-timezone", "wrong-run", "wrong-kind", "zero-sequence"} {
		t.Run(reason, func(t *testing.T) {
			inject := false
			reader := finalizationReaderFunc(func(ctx context.Context, id string) (ledger.Record, bool, error) {
				f := rows[3]
				if !inject {
					return f, true, nil
				}
				switch reason {
				case "io":
					return ledger.Record{}, false, lookupErr
				case "context":
					return ledger.Record{}, false, context.Canceled
				case "wrong-request":
					f.Event.RequestID = "other"
				case "wrong-project":
					f.Event.ProjectID = "other"
				case "wrong-period":
					f.Event.PeriodID = "other"
				case "wrong-timezone":
					f.Event.PeriodTimezoneVersion++
				case "wrong-run":
					f.Event.RunID = "other"
				case "wrong-kind":
					f.Event.Kind = ledger.EventRequestAccepted
				case "zero-sequence":
					f.Sequence = 0
				}
				return f, true, nil
			})
			a, err := NewAggregateWithFinalizationReader(testFinalizationReader{reader, rows})
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows[:4] {
				if err := a.Apply(r); err != nil {
					t.Fatal(err)
				}
			}
			before, metrics := a.Snapshot(), a.Metrics()
			inject = true
			err = a.Apply(rows[4])
			if err == nil {
				t.Fatal("failed lookup or inconsistent finalization became absence")
			}
			if reason == "io" && !errors.Is(err, lookupErr) {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(a.Snapshot(), before) || a.Metrics() != metrics {
				t.Fatal("error mutated usage")
			}
		})
	}
	// Skipping the earlier terminal fact cannot quietly create a late summary.
	a, err := NewAggregateWithFinalizationReader(fixtureFinalReader(rows))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows[:3] {
		if err := a.Apply(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Apply(rows[4]); err == nil {
		t.Fatal("usage skipped the finalization fact")
	}
}

func TestFinalizationReaderFreeAndUnknownLateSettlement(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint("unknown=", unknown), func(t *testing.T) {
			rows := finalizationFixture(true, false)
			if unknown {
				snapshot := domain.NewUnknownPriceSnapshot(rows[4].Event.OccurredAt)
				rows[4].Event.PriceSnapshot = &snapshot
				rows[4].Event.LeaseMode = ledger.LeaseModeUnknownAllowed
				rows[4].Event.CommittedMicrosUSD = nil
			} else {
				rows[4].Event.PriceSnapshot = usagePriceSnapshot(t, domain.BillingModeFree, rows[4].Event.OccurredAt)
				rows[4].Event.LeaseMode = ledger.LeaseModeFree
				rows[4].Event.CommittedMicrosUSD = ledger.MicrosUSD(0)
			}
			a, err := NewAggregateWithFinalizationReader(fixtureFinalReader(rows))
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				if err := a.Apply(r); err != nil {
					t.Fatal(err)
				}
			}
			snap := a.Snapshot()
			if len(snap.Attempts) != 1 || snap.Totals.Requests != 1 || a.Metrics().ActiveRequests != 0 {
				t.Fatal(snap, a.Metrics())
			}
			if unknown && (snap.Attempts[0].CostMicrosUSD != nil || snap.Totals.UnknownAttempts != 1) {
				t.Fatal(snap)
			}
			if !unknown && (snap.Attempts[0].CostMicrosUSD == nil || *snap.Attempts[0].CostMicrosUSD != 0 || snap.Totals.UnknownAttempts != 0) {
				t.Fatal(snap)
			}
		})
	}
}

func TestFinalizationReaderAuthenticLedgerProjectionSuppliesOrderingEvidence(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint("late=", late), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "ledger.wal")
			key := bytes.Repeat([]byte{0x42}, 32)
			log, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range finalizationFixture(late, false) {
				if _, err := log.Append(context.Background(), r.Event); err != nil {
					t.Fatal(err)
				}
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			var rows []ledger.Record
			report, partial, err := ledger.InspectReplayAuthenticated(path, key, func(r ledger.Record) error { rows = append(rows, r); return nil })
			if err != nil || partial {
				t.Fatal(partial, err)
			}
			projection, err := ledger.BuildLedgerProjection(context.Background(), path, key, filepath.Join(root, "projection.db"), report)
			if err != nil {
				t.Fatal(err)
			}
			defer projection.Close()
			a, err := NewAggregateWithFinalizationReader(projection)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				if err := a.Apply(r); err != nil {
					t.Fatal(err)
				}
			}
			snap := a.Snapshot()
			if len(snap.Attempts) != 1 || len(snap.Requests) != 1 || a.Metrics().ActiveRequests != 0 {
				t.Fatal(snap, a.Metrics())
			}
			if late && snap.Requests[0].Attempts != 0 {
				t.Fatal(snap.Requests)
			}
			if !late && snap.Requests[0].Attempts != 1 {
				t.Fatal(snap.Requests)
			}
			if err := projection.Close(); err != nil {
				t.Fatal(err)
			}
			next := rows[len(rows)-1]
			next.Sequence++
			next.Event.EventID = "new_settlement_after_closed_reader"
			next.Event.Kind = ledger.EventAttemptSettled
			next.Event.AttemptID = "att"
			next.Event.CommittedMicrosUSD = ledger.MicrosUSD(7)
			if err := a.Apply(next); !errors.Is(err, ledger.ErrProjectionUnavailable) {
				t.Fatal(err)
			}
		})
	}
}

func TestFinalizationReaderDuplicateBeyondTailWindowAndCheckpoint(t *testing.T) {
	rows := finalizationFixture(true, false)
	for i := 0; i < maxTrackedEventIDs+10; i++ {
		e := rows[0].Event
		e.EventID = fmt.Sprint("ignored_", i)
		e.Kind = ledger.EventWorkUnitCreated
		e.WorkUnitID = fmt.Sprint("wu_", i)
		e.Operation = "create"
		e.IdempotencyKeyHash = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		e.RequestFingerprint = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		seq := uint64(len(rows) + 1)
		rows = append(rows, ledger.Record{Generation: 1, Sequence: seq, Offset: int64(seq * 100), Event: e})
	}
	for _, restore := range []bool{false, true} {
		t.Run(fmt.Sprint("checkpoint=", restore), func(t *testing.T) {
			reader := fixtureFinalReader(rows)
			a, err := NewAggregateWithFinalizationReader(reader)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				if err := a.Apply(r); err != nil {
					t.Fatal(err)
				}
			}
			if restore {
				store := newCheckpointStore()
				store.round(t, a)
				a, err = RestoreCheckpointWithFinalizationReader(store.head, store.read, reader)
				if err != nil {
					t.Fatal(err)
				}
			}
			before, metrics := a.Snapshot(), a.Metrics()
			duplicate := rows[4]
			duplicate.Sequence = uint64(len(rows) + 1)
			duplicate.Offset = int64(duplicate.Sequence * 100)
			if err := a.Apply(duplicate); err != nil {
				t.Fatal(err)
			}
			after := a.Snapshot()
			before.Watermark = after.Watermark
			if !reflect.DeepEqual(before, after) || metrics != a.Metrics() {
				t.Fatal("evicted recovery event was counted twice")
			}
		})
	}
}

func TestFinalizationReaderAuthenticDuplicateBeyondTailWindowAndCheckpoint(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "ledger.wal")
	key := bytes.Repeat([]byte{0x43}, 32)
	log, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	fixture := finalizationFixture(true, false)
	for _, row := range fixture {
		if _, err := log.Append(context.Background(), row.Event); err != nil {
			t.Fatal(err)
		}
	}
	// Distinct physical governance events evict the settlement ID. These are
	// valid idempotency aliases, so rebuilding the real projection also checks
	// the distinction between a new EventID and a repeated domain operation.
	for i := 0; i < maxTrackedEventIDs+10; i++ {
		e := fixture[0].Event
		e.Kind, e.EventID, e.WorkUnitID = ledger.EventWorkUnitCreated, fmt.Sprint("alias_", i), "wku_native"
		e.Operation = "create"
		e.IdempotencyKeyHash = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		e.RequestFingerprint = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		if _, err := log.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := log.Append(context.Background(), fixture[4].Event); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	// This slice is a test oracle, not the production recovery implementation.
	var rows []ledger.Record
	report, partial, err := ledger.InspectReplayAuthenticated(path, key, func(r ledger.Record) error { rows = append(rows, r); return nil })
	if err != nil || partial {
		t.Fatal(partial, err)
	}
	p, err := ledger.BuildLedgerProjection(context.Background(), path, key, filepath.Join(root, "projection.db"), report)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	first, found, err := p.ReadEventPosition(context.Background(), fixture[4].Event.EventID)
	if err != nil || !found || first.Sequence != 5 {
		t.Fatal(first, found, err)
	}
	a, err := NewAggregateWithFinalizationReader(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows[:len(rows)-1] {
		if err := a.Apply(row); err != nil {
			t.Fatal(err)
		}
	}
	store := newCheckpointStore()
	store.round(t, a)
	a, err = RestoreCheckpointWithFinalizationReader(store.head, store.read, p)
	if err != nil {
		t.Fatal(err)
	}
	before, metrics := a.Snapshot(), a.Metrics()
	if err := a.Apply(rows[len(rows)-1]); err != nil {
		t.Fatal(err)
	}
	after := a.Snapshot()
	if after.Watermark.Sequence != rows[len(rows)-1].Sequence {
		t.Fatal("duplicate physical position not consumed", after.Watermark)
	}
	before.Watermark = after.Watermark
	if !reflect.DeepEqual(before, after) || a.Metrics() != metrics || metrics.ActiveRequests != 0 || len(after.Attempts) != 1 || after.Totals.CostMicrosUSD != 7 {
		t.Fatal("authenticated duplicate changed Usage effects", after, a.Metrics())
	}
}
