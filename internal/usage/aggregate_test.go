package usage

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/ledger"
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
				_, payload, err := prefix.MarshalCheckpoint()
				if err != nil {
					t.Fatal(err)
				}
				recovered, err = RestoreCheckpoint(payload)
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

func checkpointKillPointRecords(requests int) []ledger.Record {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	result := make([]ledger.Record, 0, requests*5)
	appendEvent := func(event ledger.Event) {
		sequence := len(result) + 1
		result = append(result, ledger.Record{Sequence: uint64(sequence), Offset: int64(sequence * 256), Event: event})
	}
	for index := 0; index < requests; index++ {
		requestID, attemptID := fmt.Sprintf("req_%03d", index), fmt.Sprintf("att_%03d", index)
		base := now.Add(time.Duration(index) * time.Second)
		common := ledger.Event{RequestID: requestID, ProjectID: "project_1", KeyID: "key_1", RequestedModel: "chat", PeriodID: "2026-07-31"}
		event := common
		event.EventID, event.Kind, event.OccurredAt = "accept_"+requestID, ledger.EventRequestAccepted, base
		appendEvent(event)
		event = common
		event.EventID, event.Kind, event.AttemptID, event.OccurredAt, event.ReservationMicrosUSD = "reserve_"+requestID, ledger.EventReservationCreated, attemptID, base.Add(time.Millisecond), 100
		appendEvent(event)
		event = common
		event.EventID, event.Kind, event.AttemptID, event.OccurredAt = "start_"+requestID, ledger.EventAttemptStarted, attemptID, base.Add(2*time.Millisecond)
		appendEvent(event)
		event = common
		event.EventID, event.Kind, event.AttemptID, event.OccurredAt = "settle_"+requestID, ledger.EventAttemptSettled, attemptID, base.Add(10*time.Millisecond)
		event.CommittedMicrosUSD, event.ProviderInputTokens, event.ProviderOutputTokens = 80, 7, 3
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
			ProjectID: "p", PeriodID: "2026-07-31", OccurredAt: now, ReservationMicrosUSD: 10},
		{EventID: "started", Kind: ledger.EventAttemptStarted, RequestID: "req_1", AttemptID: "a1",
			ProjectID: "p", PeriodID: "2026-07-31", OccurredAt: now},
		{EventID: "settled", Kind: ledger.EventAttemptSettled, RequestID: "req_1", AttemptID: "a1",
			ProjectID: "p", KeyID: "k", RouteID: "r", ProviderID: "provider",
			RequestedModel: "chat", ProviderModel: "model", AttemptNumber: 1,
			PeriodID: "2026-07-31", OccurredAt: now.Add(time.Second),
			CommittedMicrosUSD: 7, ProviderInputTokens: 3, ProviderOutputTokens: 2,
			Outcome: "success", HTTPStatus: 200, LatencyMillis: 1000},
		{EventID: "final", Kind: ledger.EventRequestFinalized, RequestID: "req_1", ProjectID: "p",
			KeyID: "k", RequestedModel: "chat", PeriodID: "2026-07-31",
			OccurredAt: now.Add(time.Second), Outcome: "success"},
	}
	for index, event := range events {
		record := ledger.Record{Sequence: uint64(index + 1), Offset: int64(index + 1), Event: event}
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
			Sequence: uint64(index + 1), Offset: int64((index + 1) * 100), Event: event,
		}); err != nil {
			t.Fatal(err)
		}
	}
	watermark, payload, err := aggregate.MarshalCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreCheckpoint(payload)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Snapshot().Watermark != watermark {
		t.Fatalf("restored watermark=%#v want=%#v", restored.Snapshot().Watermark, watermark)
	}
	for index, event := range []ledger.Event{
		{EventID: "settled", Kind: ledger.EventAttemptSettled, RequestID: "req_1",
			AttemptID: "a1", ProjectID: "p", PeriodID: "2026-07-31",
			OccurredAt: now.Add(time.Second), CommittedMicrosUSD: 7,
			ProviderInputTokens: 3, ProviderOutputTokens: 2, Outcome: "success",
			LatencyMillis: 999},
		{EventID: "final", Kind: ledger.EventRequestFinalized, RequestID: "req_1",
			ProjectID: "p", PeriodID: "2026-07-31", OccurredAt: now.Add(time.Second),
			Outcome: "success"},
	} {
		if err := restored.Apply(ledger.Record{
			Sequence: uint64(index + 3), Offset: int64((index + 3) * 100), Event: event,
		}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := restored.Snapshot()
	if snapshot.Totals.Requests != 1 || snapshot.Totals.Attempts != 1 ||
		len(snapshot.Requests) != 1 || snapshot.Requests[0].KeyID != "k" ||
		snapshot.Attempts[0].StartedAt != now.Add(time.Millisecond) {
		t.Fatalf("restored snapshot=%#v", snapshot)
	}
}
