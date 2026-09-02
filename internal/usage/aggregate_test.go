package usage

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

func TestCheckpointRecoveryMatchesFullReplayAcrossOneHundredKillPoints(t *testing.T) {
	records := checkpointKillPointRecords(25) // 125 durable records, 126 boundaries.
	expected := NewAggregate()
	for _, record := range records {
		if err := expected.Apply(record); err != nil {
			t.Fatal(err)
		}
	}
	expectedSnapshot, expectedMetrics := expected.Snapshot(), expected.Metrics()
	for killPoint := 0; killPoint <= len(records); killPoint++ {
		for _, checkpointCommitted := range []bool{false, true} {
			checkpointEnd := killPoint
			if !checkpointCommitted && checkpointEnd > 0 {
				checkpointEnd--
			}
			prefix := NewAggregate()
			for _, record := range records[:checkpointEnd] {
				if err := prefix.Apply(record); err != nil {
					t.Fatalf("kill=%d committed=%t prefix: %v", killPoint, checkpointCommitted, err)
				}
			}
			recovered := NewAggregate()
			if checkpointEnd > 0 {
				snapshot, err := prefix.TakeCheckpoint()
				if err != nil {
					t.Fatal(err)
				}
				recovered, err = RestoreCheckpoint(snapshot.Payload)
				if err != nil {
					t.Fatalf("kill=%d committed=%t restore: %v", killPoint, checkpointCommitted, err)
				}
			}
			for _, record := range records[checkpointEnd:] {
				if err := recovered.Apply(record); err != nil {
					t.Fatalf("kill=%d committed=%t replay: %v", killPoint, checkpointCommitted, err)
				}
			}
			if actual := recovered.Snapshot(); !reflect.DeepEqual(actual, expectedSnapshot) {
				t.Fatalf("kill=%d committed=%t snapshot mismatch\nactual=%#v\nexpected=%#v", killPoint, checkpointCommitted, actual, expectedSnapshot)
			}
			if actual := recovered.Metrics(); actual != expectedMetrics {
				t.Fatalf("kill=%d committed=%t metrics=%#v expected=%#v", killPoint, checkpointCommitted, actual, expectedMetrics)
			}
		}
	}
}

func TestAggregateRejectsInt64Overflow(t *testing.T) {
	aggregate := NewAggregate()
	aggregate.totals.CostMicrosUSD = math.MaxInt64
	event := ledger.Event{EventID: "overflow", Kind: ledger.EventAttemptSettled, RequestID: "req", AttemptID: "att", ProjectID: "p", PeriodID: "2026-08-04", OccurredAt: time.Now().UTC(), CommittedMicrosUSD: ledger.MicrosUSD(1), Outcome: "success"}
	if err := aggregate.Apply(ledger.Record{Generation: 1, Sequence: 1, Offset: 1, Event: event}); err == nil {
		t.Fatal("expected usage aggregate overflow")
	}
}

func usagePriceSnapshot(t *testing.T, mode domain.BillingMode, selectedAt time.Time) *domain.PriceSnapshot {
	t.Helper()
	price := domain.DeploymentPriceVersion{ID: "price_usage", DeploymentID: "dep_usage", Version: 1, Revision: 1,
		BillingMode: mode, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1, EffectiveFrom: selectedAt.Add(-time.Hour),
		CreatedBy: "test", CreatedAt: selectedAt.Add(-time.Hour), Source: domain.PriceSource{Type: domain.PriceSourceManual,
			Assurance: domain.PriceAssuranceAsserted, ReceivedAt: selectedAt.Add(-time.Hour), Reference: "test", AssertedWithoutArchive: true,
			ContentSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	if mode == domain.BillingModeMetered {
		price.InputMicrosPerMillion, price.OutputMicrosPerMillion = 1_000_000, 2_000_000
	}
	snapshot, err := domain.NewVersionedPriceSnapshot(price, selectedAt)
	if err != nil {
		t.Fatal(err)
	}
	return &snapshot
}

func TestAggregatePreservesKnownFreeUnknownAndLegacySemantics(t *testing.T) {
	aggregate := NewAggregate()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	free := usagePriceSnapshot(t, domain.BillingModeFree, now)
	unknown := domain.NewUnknownPriceSnapshot(now)
	events := []ledger.Event{
		{EventID: "legacy", Kind: ledger.EventAttemptSettled, RequestID: "req_legacy", AttemptID: "att_legacy", ProjectID: "p", PeriodID: "2026-08-04", OccurredAt: now, CommittedMicrosUSD: ledger.MicrosUSD(7), Outcome: "success"},
		{EventID: "free", Kind: ledger.EventAttemptSettled, RequestID: "req_free", AttemptID: "att_free", ProjectID: "p", PeriodID: "2026-08-04", OccurredAt: now.Add(time.Minute), CommittedMicrosUSD: ledger.MicrosUSD(0), LeaseMode: ledger.LeaseModeFree, PriceSnapshot: free, Outcome: "success", TokenUsageSource: ledger.TokenUsageSourceProvider},
		{EventID: "unknown", Kind: ledger.EventAttemptSettled, RequestID: "req_unknown", AttemptID: "att_unknown", ProjectID: "p", PeriodID: "2026-08-04", OccurredAt: now.Add(2 * time.Minute), LeaseMode: ledger.LeaseModeUnknownAllowed, PriceSnapshot: &unknown, Outcome: "success", TokenUsageSource: ledger.TokenUsageSourceProvider},
	}
	for index, event := range events {
		if err := aggregate.Apply(ledger.Record{Generation: 1, Sequence: uint64(index + 1), Offset: int64(index + 1), Event: event}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := aggregate.Snapshot()
	if snapshot.Totals.CostMicrosUSD != 7 || snapshot.Totals.UnknownAttempts != 1 {
		t.Fatalf("totals=%#v", snapshot.Totals)
	}
	byID := map[string]AttemptEvent{}
	for _, attempt := range snapshot.Attempts {
		byID[attempt.AttemptID] = attempt
	}
	if !containsTag(byID["att_legacy"].Tags, "LEGACY") || *byID["att_legacy"].CostMicrosUSD != 7 ||
		!containsTag(byID["att_free"].Tags, "FREE") || !containsTag(byID["att_unknown"].Tags, "UNKNOWN") || byID["att_unknown"].CostMicrosUSD != nil {
		t.Fatalf("attempts=%#v", byID)
	}
	encoded, err := json.Marshal(byID["att_unknown"])
	if err != nil || !containsJSONNull(encoded, `"cost_micros_usd":null`) {
		t.Fatalf("unknown json=%s err=%v", encoded, err)
	}
}

func containsJSONNull(encoded []byte, wanted string) bool {
	return string(encoded) != "" && len(encoded) >= len(wanted) && func() bool {
		for i := 0; i+len(wanted) <= len(encoded); i++ {
			if string(encoded[i:i+len(wanted)]) == wanted {
				return true
			}
		}
		return false
	}()
}

func checkpointKillPointRecords(requests int) []ledger.Record {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	result := make([]ledger.Record, 0, requests*5)
	appendEvent := func(event ledger.Event) {
		sequence := len(result) + 1
		result = append(result, ledger.Record{Generation: 1, Sequence: uint64(sequence), Offset: int64(sequence * 256), Event: event})
	}
	for index := 0; index < requests; index++ {
		requestID, attemptID := fmt.Sprintf("req_%03d", index), fmt.Sprintf("att_%03d", index)
		base := now.Add(time.Duration(index) * time.Second)
		common := ledger.Event{RequestID: requestID, ProjectID: "project_1", KeyID: "key_1", RequestedModel: "chat", PeriodID: "2026-07-31"}
		event := common
		event.EventID, event.Kind, event.OccurredAt = "accept_"+requestID, ledger.EventRequestAccepted, base
		appendEvent(event)
		event = common
		event.EventID, event.Kind, event.AttemptID, event.OccurredAt, event.ReservationMicrosUSD = "reserve_"+requestID, ledger.EventReservationCreated, attemptID, base.Add(time.Millisecond), ledger.MicrosUSD(100)
		appendEvent(event)
		event = common
		event.EventID, event.Kind, event.AttemptID, event.OccurredAt = "start_"+requestID, ledger.EventAttemptStarted, attemptID, base.Add(2*time.Millisecond)
		appendEvent(event)
		event = common
		event.EventID, event.Kind, event.AttemptID, event.OccurredAt = "settle_"+requestID, ledger.EventAttemptSettled, attemptID, base.Add(10*time.Millisecond)
		event.CommittedMicrosUSD, event.ProviderInputTokens, event.ProviderOutputTokens = ledger.MicrosUSD(80), 7, 3
		event.Outcome, event.HTTPStatus, event.LatencyMillis, event.AttemptNumber = "success", 200, 8, 1
		appendEvent(event)
		event = common
		event.EventID, event.Kind, event.OccurredAt, event.Outcome = "final_"+requestID, ledger.EventRequestFinalized, base.Add(11*time.Millisecond), "success"
		appendEvent(event)
	}
	return result
}

func TestAggregateBuildsAttemptAndRequestViewsIdempotently(t *testing.T) {
	aggregate := NewAggregate()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	events := []ledger.Event{
		{EventID: "accepted", Kind: ledger.EventRequestAccepted, RequestID: "req_1", ProjectID: "p",
			KeyID: "k", RequestedModel: "chat", PeriodID: "2026-07-31", OccurredAt: now},
		{EventID: "reserved", Kind: ledger.EventReservationCreated, RequestID: "req_1", AttemptID: "a1",
			ProjectID: "p", PeriodID: "2026-07-31", OccurredAt: now, ReservationMicrosUSD: ledger.MicrosUSD(10)},
		{EventID: "started", Kind: ledger.EventAttemptStarted, RequestID: "req_1", AttemptID: "a1",
			ProjectID: "p", PeriodID: "2026-07-31", OccurredAt: now},
		{EventID: "settled", Kind: ledger.EventAttemptSettled, RequestID: "req_1", AttemptID: "a1",
			ProjectID: "p", KeyID: "k", RouteID: "r", ProviderID: "provider",
			RequestedModel: "chat", ProviderModel: "model", AttemptNumber: 1,
			PeriodID: "2026-07-31", OccurredAt: now.Add(time.Second),
			CommittedMicrosUSD: ledger.MicrosUSD(7), ProviderInputTokens: 3, ProviderOutputTokens: 2,
			Outcome: "success", HTTPStatus: 200, LatencyMillis: 1000},
		{EventID: "final", Kind: ledger.EventRequestFinalized, RequestID: "req_1", ProjectID: "p",
			KeyID: "k", RequestedModel: "chat", PeriodID: "2026-07-31",
			OccurredAt: now.Add(time.Second), Outcome: "success"},
	}
	for index, event := range events {
		record := ledger.Record{Generation: 1, Sequence: uint64(index + 1), Offset: int64(index + 1), Event: event}
		if err := aggregate.Apply(record); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			if err := aggregate.Apply(record); err != nil {
				t.Fatal(err)
			}
		}
	}
	snapshot := aggregate.Snapshot()
	if snapshot.Totals.Requests != 1 || snapshot.Totals.Attempts != 1 ||
		snapshot.Totals.InputTokens != 3 || snapshot.Totals.OutputTokens != 2 ||
		snapshot.Totals.CostMicrosUSD != 7 {
		t.Fatalf("totals=%#v", snapshot.Totals)
	}
	if len(snapshot.Attempts) != 1 || len(snapshot.Requests) != 1 ||
		snapshot.Attempts[0].ProviderID != "provider" || snapshot.Requests[0].Attempts != 1 {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	metrics := aggregate.Metrics()
	if metrics.RequestsSuccess != 1 || metrics.AttemptsSuccess != 1 ||
		metrics.InputTokens != 3 || metrics.OutputTokens != 2 ||
		metrics.CostMicrosUSD != 7 || metrics.ActiveRequests != 0 {
		t.Fatalf("metrics=%#v", metrics)
	}
	if metrics.AttemptLatencyBuckets[6] != 1 || metrics.RequestLatencyBuckets[6] != 1 {
		t.Fatalf("histogram buckets=%#v %#v", metrics.AttemptLatencyBuckets, metrics.RequestLatencyBuckets)
	}
}

func TestCheckpointRestoresActiveRequestAndContinuesMonotonically(t *testing.T) {
	aggregate := NewAggregate()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	for index, event := range []ledger.Event{
		{EventID: "accepted", Kind: ledger.EventRequestAccepted, RequestID: "req_1",
			ProjectID: "p", KeyID: "k", RequestedModel: "chat",
			PeriodID: "2026-07-31", OccurredAt: now},
		{EventID: "started", Kind: ledger.EventAttemptStarted, RequestID: "req_1",
			AttemptID: "a1", ProjectID: "p", PeriodID: "2026-07-31",
			OccurredAt: now.Add(time.Millisecond)},
	} {
		if err := aggregate.Apply(ledger.Record{
			Generation: 1,
			Sequence:   uint64(index + 1), Offset: int64((index + 1) * 100), Event: event,
		}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := aggregate.TakeCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	watermark := snapshot.Watermark
	restored, err := RestoreCheckpoint(snapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Snapshot().Watermark != watermark {
		t.Fatalf("restored watermark=%#v want=%#v", restored.Snapshot().Watermark, watermark)
	}
	for index, event := range []ledger.Event{
		{EventID: "settled", Kind: ledger.EventAttemptSettled, RequestID: "req_1",
			AttemptID: "a1", ProjectID: "p", PeriodID: "2026-07-31",
			OccurredAt: now.Add(time.Second), CommittedMicrosUSD: ledger.MicrosUSD(7),
			ProviderInputTokens: 3, ProviderOutputTokens: 2, Outcome: "success",
			LatencyMillis: 999},
		{EventID: "final", Kind: ledger.EventRequestFinalized, RequestID: "req_1",
			ProjectID: "p", PeriodID: "2026-07-31", OccurredAt: now.Add(time.Second),
			Outcome: "success"},
	} {
		if err := restored.Apply(ledger.Record{
			Generation: 1,
			Sequence:   uint64(index + 3), Offset: int64((index + 3) * 100), Event: event,
		}); err != nil {
			t.Fatal(err)
		}
	}
	state := restored.Snapshot()
	if state.Totals.Requests != 1 || state.Totals.Attempts != 1 ||
		len(state.Requests) != 1 || state.Requests[0].KeyID != "k" ||
		state.Attempts[0].StartedAt != now.Add(time.Millisecond) {
		t.Fatalf("restored snapshot=%#v", state)
	}
}

// TestCheckpointCarriesTheDedupWindow covers a disagreement between the
// aggregate and a full replay of the same WAL. Crash recovery deliberately
// re-emits a deterministic event rather than inventing a new ID, so one event
// ID can occupy two physical frames. Apply skips the second because it
// remembers the first — but the checkpoint did not carry that memory, so an
// aggregate restored between the two frames added the cost a second time.
func TestCheckpointCarriesTheDedupWindow(t *testing.T) {
	aggregate := NewAggregate()
	settled := ledger.Record{Generation: 1, Sequence: 1, Offset: 100, Event: ledger.Event{
		EventID: "evt_settled", Kind: ledger.EventAttemptSettled,
		RequestID: "req_1", AttemptID: "att_1", ProjectID: "prj_1",
		PeriodID: "prj_1:2026-08-07", OccurredAt: time.Now().UTC(),
		CommittedMicrosUSD: ledger.MicrosUSD(7), Outcome: "success",
	}}
	if err := aggregate.Apply(settled); err != nil {
		t.Fatal(err)
	}
	before := aggregate.Snapshot().Totals

	snapshot, err := aggregate.TakeCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreCheckpoint(snapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	// The same event at a later sequence, exactly as the recovery path writes it.
	duplicate := settled
	duplicate.Sequence = 2
	duplicate.Offset = 200
	if err := restored.Apply(duplicate); err != nil {
		t.Fatal(err)
	}
	after := restored.Snapshot().Totals
	if after.CostMicrosUSD != before.CostMicrosUSD || after.Attempts != before.Attempts {
		t.Fatalf("a re-emitted event was counted again after restore: cost=%d attempts=%d, want cost=%d attempts=%d",
			after.CostMicrosUSD, after.Attempts, before.CostMicrosUSD, before.Attempts)
	}
}
