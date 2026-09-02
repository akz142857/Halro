package usage

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

// The identifiers an operator quotes to an upstream's support desk. They lived
// only in the process log, which rotates and is not the record, so a ticket
// raised a week after the failure had nothing to name the request by.
//
// This checks the whole derivation: a ledger event carries them, the aggregate
// keeps them, the failed-request list surfaces them, a checkpoint round-trip
// preserves them, and a rebuild from the same events lands on the same values.
func failureIdentifierEvents(now time.Time) []ledger.Event {
	return []ledger.Event{
		{
			EventID: "req_accepted", Kind: ledger.EventRequestAccepted,
			RequestID: "req_1", ProjectID: "project_1", PeriodID: "2026-08-22",
			RequestedModel: "chat", OccurredAt: now,
		},
		{
			EventID: "att_settled", Kind: ledger.EventAttemptSettled,
			RequestID: "req_1", AttemptID: "att_1", AttemptNumber: 1,
			ProjectID: "project_1", PeriodID: "2026-08-22", ProviderID: "provider_1",
			DeploymentID: "dep_1", ProviderModel: "gpt-4o", RequestedModel: "chat",
			OccurredAt: now, Outcome: "provider_error", ErrorClass: "bad_request",
			HTTPStatus: 400, CommittedMicrosUSD: ledger.MicrosUSD(1),
			ProviderCode:      "invalid_image_url:messages[0].content[1].image_url",
			ProviderRequestID: "upstream-req-42", FailurePhase: "provider",
		},
		{
			EventID: "req_final", Kind: ledger.EventRequestFinalized,
			RequestID: "req_1", ProjectID: "project_1", PeriodID: "2026-08-22",
			RequestedModel: "chat", OccurredAt: now.Add(time.Second), Outcome: "provider_error",
		},
	}
}

func applyEvents(t *testing.T, aggregate *Aggregate, events []ledger.Event) {
	t.Helper()
	for index, event := range events {
		if err := aggregate.Apply(ledger.Record{
			Generation: 1,
			Sequence:   uint64(index + 1), Offset: int64((index + 1) * 100), Event: event,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProviderIdentifiersSurviveTheDerivation(t *testing.T) {
	now := time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC)
	aggregate := NewAggregate()
	applyEvents(t, aggregate, failureIdentifierEvents(now))

	detail, exists := aggregate.RequestDetail("req_1")
	if !exists || len(detail.Attempts) != 1 {
		t.Fatalf("detail = %#v", detail)
	}
	attempt := detail.Attempts[0]
	if attempt.ProviderCode != "invalid_image_url:messages[0].content[1].image_url" ||
		attempt.ProviderRequestID != "upstream-req-42" || attempt.FailurePhase != "provider" {
		t.Fatalf("the attempt lost the upstream's identifiers: %#v", attempt)
	}

	page, err := aggregate.QueryFailedRequests(FailureQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Failures) != 1 || page.Failures[0].LastFailure == nil {
		t.Fatalf("failures = %#v", page.Failures)
	}
	last := page.Failures[0].LastFailure
	if last.ProviderCode != attempt.ProviderCode ||
		last.ProviderRequestID != attempt.ProviderRequestID ||
		last.FailurePhase != attempt.FailurePhase {
		t.Fatalf("the failed-request row lost them: %#v", last)
	}
}

// Rebuilding from the ledger has to land where the incremental path did, or the
// checkpoint has become a second source of truth for these fields.
func TestProviderIdentifiersMatchAfterCheckpointAndRebuild(t *testing.T) {
	now := time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC)
	events := failureIdentifierEvents(now)

	incremental := NewAggregate()
	applyEvents(t, incremental, events)
	restored, err := restoreOneRound(incremental)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := NewAggregate()
	applyEvents(t, rebuilt, events)

	for name, aggregate := range map[string]*Aggregate{"restored": restored, "rebuilt": rebuilt} {
		detail, exists := aggregate.RequestDetail("req_1")
		if !exists || len(detail.Attempts) != 1 {
			t.Fatalf("%s: detail = %#v", name, detail)
		}
		if !reflect.DeepEqual(detail.Attempts[0], incremental.attempts[0]) {
			t.Fatalf("%s attempt differs from the incremental one:\n%#v\n%#v",
				name, detail.Attempts[0], incremental.attempts[0])
		}
	}
}

// A partition written before these columns existed still reads, and reads as
// "not recorded" rather than as an upstream that named nothing. The verifier
// compares rows at the version they were written at, so a bump must not fail
// verification for exactly the installs that have history.
func TestParquetKeepsAndNarrowsTheProviderIdentifiers(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	exporter, err := NewExporter(root)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	snapshot := Snapshot{Attempts: []AttemptEvent{{
		EventID: "event_1", RequestID: "request_1", AttemptID: "attempt_1",
		Sequence: 4, AttemptNumber: 1, ProjectID: "project_1", KeyID: "key_1",
		RouteID: "route_1", ProviderID: "provider_1", RequestedModel: "chat",
		ProviderModel: "model_1", ProviderInputTokens: 3, ProviderOutputTokens: 2,
		CostMicrosUSD: ledger.MicrosUSD(7), StartedAt: day.Add(-time.Second),
		CompletedAt: day, Status: "provider_error", ErrorClass: "bad_request",
		HTTPStatus: 400, LatencyMillis: 1000,
		ProviderCode: "invalid_image_url", ProviderRequestID: "upstream-req-42",
		FailurePhase: "provider",
	}}}
	if _, err := exporter.Export(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := exporter.Verify(&snapshot); err != nil {
		t.Fatalf("a partition holding the new columns did not verify: %v", err)
	}

	row := toParquetAttempt(snapshot.Attempts[0])
	if row.SchemaVersion != parquetSchemaVersion || row.ProviderCode != "invalid_image_url" ||
		row.ProviderRequestID != "upstream-req-42" || row.FailurePhase != "provider" {
		t.Fatalf("row = %#v", row)
	}
	// What a row written by the version before this one decodes as. Narrowed to
	// empty rather than kept, so the comparison the verifier makes against a
	// schema-4 partition still holds.
	narrowed := narrowToSchema(row, 4)
	if narrowed.ProviderCode != "" || narrowed.ProviderRequestID != "" || narrowed.FailurePhase != "" {
		t.Fatalf("a schema-4 row was compared against schema-5 columns: %#v", narrowed)
	}
	// And the columns the version before that never had are still narrowed too:
	// the ladder has to keep every rung, not only the newest.
	older := narrowToSchema(row, 3)
	if older.ProviderCode != "" || older.ProviderReasoningTokens != 0 {
		t.Fatalf("schema 3 kept a column it never had: %#v", older)
	}
}
