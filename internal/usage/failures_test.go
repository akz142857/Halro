package usage

import (
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

// failureFixture builds an aggregate holding one request of each shape the
// failed-request list has to answer for, in the order they were admitted:
//
//	req_ok       — succeeded on its first attempt
//	req_fallback — failed once, then succeeded on another target
//	req_failed   — failed on both targets
//	req_rejected — admitted, then refused before any upstream call
//
// The counts these produce are the ones TestFailureTaxonomyIsTheSameInEveryView
// pins in the gateway; this is the same taxonomy seen from the read side.
func failureFixture(t *testing.T) *Aggregate {
	t.Helper()
	aggregate := NewAggregate()
	now := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	var sequence uint64
	apply := func(event ledger.Event) {
		t.Helper()
		sequence++
		if err := aggregate.Apply(ledger.Record{
			Generation: 1,
			Sequence:   sequence, Offset: int64(sequence * 100), Event: event,
		}); err != nil {
			t.Fatal(err)
		}
	}
	attempt := func(requestID, attemptID string, number int, at time.Time, outcome, class string, status int, provider, deployment, model string) ledger.Event {
		return ledger.Event{
			EventID: attemptID + "_settled", Kind: ledger.EventAttemptSettled,
			RequestID: requestID, AttemptID: attemptID, AttemptNumber: number,
			ProjectID: "project", PeriodID: "period", ProviderID: provider,
			DeploymentID: deployment, ProviderModel: model, RequestedModel: "chat",
			OccurredAt: at, Outcome: outcome, ErrorClass: class, HTTPStatus: status,
			CommittedMicrosUSD: ledger.MicrosUSD(1),
		}
	}
	request := func(requestID string, at time.Time, outcome string, attempts ...ledger.Event) {
		apply(ledger.Event{
			EventID: requestID + "_accepted", Kind: ledger.EventRequestAccepted,
			RequestID: requestID, ProjectID: "project", PeriodID: "period",
			RequestedModel: "chat", OccurredAt: at,
		})
		for _, event := range attempts {
			apply(event)
		}
		apply(ledger.Event{
			EventID: requestID + "_final", Kind: ledger.EventRequestFinalized,
			RequestID: requestID, ProjectID: "project", PeriodID: "period",
			RequestedModel: "chat", OccurredAt: at.Add(time.Second), Outcome: outcome,
		})
	}

	request("req_ok", now, "success",
		attempt("req_ok", "att_ok", 1, now, "success", "", 0, "provider_a", "dep_a", "model-a"))
	request("req_fallback", now.Add(time.Minute), "success",
		attempt("req_fallback", "att_fb_1", 1, now.Add(time.Minute), "provider_error", "timeout", 504, "provider_a", "dep_a", "model-a"),
		attempt("req_fallback", "att_fb_2", 2, now.Add(time.Minute), "success", "", 0, "provider_b", "dep_b", "model-b"))
	request("req_failed", now.Add(2*time.Minute), "provider_error",
		attempt("req_failed", "att_f_1", 1, now.Add(2*time.Minute), "provider_error", "provider_5xx", 503, "provider_a", "dep_a", "model-a"),
		attempt("req_failed", "att_f_2", 2, now.Add(2*time.Minute), "provider_error", "authentication", 401, "provider_b", "dep_b", "model-b"))
	request("req_rejected", now.Add(3*time.Minute), "rejected")
	return aggregate
}

// The contract the whole list is built to hold: its length equals the figure on
// the summary card. That only works if it counts requests rather than attempts
// — the fallback request failed an attempt and is not here — and if it keeps
// the rejections, which have nothing provider-shaped to show.
func TestFailedRequestListMatchesTheRequestErrorCount(t *testing.T) {
	aggregate := failureFixture(t)
	page, err := aggregate.QueryFailedRequests(FailureQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if errors := aggregate.Metrics().RequestsError; uint64(len(page.Failures)) != errors {
		t.Fatalf("list has %d rows, the summary counts %d failed requests", len(page.Failures), errors)
	}
	if len(page.Failures) != 2 {
		t.Fatalf("failures = %#v", page.Failures)
	}
	// Newest first, like the attempt list beside it.
	if page.Failures[0].RequestID != "req_rejected" || page.Failures[1].RequestID != "req_failed" {
		t.Fatalf("order = %s, %s", page.Failures[0].RequestID, page.Failures[1].RequestID)
	}
	for _, failure := range page.Failures {
		if failure.RequestID == "req_fallback" {
			t.Fatal("a request that fell back and succeeded is not a failed request")
		}
	}
}

// The row that has no provider to name must not be given one. Rendering an
// empty context reports "the upstream did not answer" for a request that never
// asked, and sends the operator to audit a deployment that was never chosen.
func TestARejectedRequestCarriesNoProviderContext(t *testing.T) {
	aggregate := failureFixture(t)
	page, err := aggregate.QueryFailedRequests(FailureQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	rejected := page.Failures[0]
	if rejected.Outcome != "rejected" {
		t.Fatalf("outcome = %q", rejected.Outcome)
	}
	if rejected.LastFailure != nil {
		t.Fatalf("a rejection was given a provider context: %#v", rejected.LastFailure)
	}
	if rejected.Attempts != 0 {
		t.Fatalf("attempts = %d, want 0", rejected.Attempts)
	}
}

// Which attempt explains the outcome: the last one that failed, not the first
// and not whichever the chain happened to end on.
func TestTheLastFailedAttemptExplainsTheRequest(t *testing.T) {
	aggregate := failureFixture(t)
	page, err := aggregate.QueryFailedRequests(FailureQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	failed := page.Failures[1]
	if failed.RequestID != "req_failed" || failed.LastFailure == nil {
		t.Fatalf("failure = %#v", failed)
	}
	last := failed.LastFailure
	if last.ErrorClass != "authentication" || last.ProviderStatus != 401 ||
		last.DeploymentID != "dep_b" || last.AttemptID != "att_f_2" {
		t.Fatalf("last failure = %#v", last)
	}
	if failed.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", failed.Attempts)
	}
}

func TestFailedRequestPagingAndFilters(t *testing.T) {
	aggregate := failureFixture(t)
	first, err := aggregate.QueryFailedRequests(FailureQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Failures) != 1 || first.Failures[0].RequestID != "req_rejected" || first.NextCursor == "" {
		t.Fatalf("first page = %#v", first)
	}
	cursor, err := DecodeCursor(first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	second, err := aggregate.QueryFailedRequests(FailureQuery{Limit: 1, BeforeSequence: cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Failures) != 1 || second.Failures[0].RequestID != "req_failed" || second.NextCursor != "" {
		t.Fatalf("second page = %#v", second)
	}

	// The filter an operator reaches for first: a caller reports an ID and wants
	// that request, not a population to narrow. Exact, so a truncated ID returns
	// nothing rather than a plausible neighbour — and it reaches the rejections
	// too, which have no attempts for an attempt-scoped filter to match.
	byRequest, err := aggregate.QueryFailedRequests(FailureQuery{Limit: 100, RequestID: "req_rejected"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byRequest.Failures) != 1 || byRequest.Failures[0].RequestID != "req_rejected" {
		t.Fatalf("request filtered = %#v", byRequest.Failures)
	}
	// A request that succeeded is not findable here by ID, because it did not
	// fail. Answering with an empty list is the correct answer to "what went
	// wrong with this one".
	bySucceeded, err := aggregate.QueryFailedRequests(FailureQuery{Limit: 100, RequestID: "req_fallback"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bySucceeded.Failures) != 0 {
		t.Fatalf("a successful request was returned as a failure: %#v", bySucceeded.Failures)
	}

	// A deployment filter is answered from the attempts, so it necessarily
	// drops the rejection: that request never chose a deployment, and
	// pretending otherwise would attribute a budget refusal to an upstream.
	byDeployment, err := aggregate.QueryFailedRequests(FailureQuery{Limit: 100, DeploymentID: "dep_b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byDeployment.Failures) != 1 || byDeployment.Failures[0].RequestID != "req_failed" {
		t.Fatalf("deployment filtered = %#v", byDeployment)
	}
	// A deployment that only served a successful attempt still has no failed
	// request of its own.
	byUnusedDeployment, err := aggregate.QueryFailedRequests(FailureQuery{Limit: 100, DeploymentID: "dep_missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byUnusedDeployment.Failures) != 0 {
		t.Fatalf("unmatched deployment returned %#v", byUnusedDeployment.Failures)
	}

	// The interval is half-open on the same rule the attempt list uses, so a
	// summary card's [start, end) links here without moving a boundary row.
	windowed, err := aggregate.QueryFailedRequests(FailureQuery{
		Limit: 100,
		Start: time.Date(2026, 8, 20, 9, 2, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 20, 9, 3, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(windowed.Failures) != 1 || windowed.Failures[0].RequestID != "req_failed" {
		t.Fatalf("windowed = %#v", windowed.Failures)
	}

	if _, err := aggregate.QueryFailedRequests(FailureQuery{Limit: 0}); err == nil {
		t.Fatal("an unbounded page was accepted")
	}
	if _, err := aggregate.QueryFailedRequests(FailureQuery{Limit: 101}); err == nil {
		t.Fatal("a page above the ceiling was accepted")
	}
}

// The cursor is a ledger sequence, so it has to survive the restart that a
// slice position would not.
func TestFailedRequestPagingSurvivesACheckpointRestore(t *testing.T) {
	aggregate := failureFixture(t)
	snapshot, err := aggregate.TakeCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreCheckpoint(snapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	before, err := aggregate.QueryFailedRequests(FailureQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	after, err := restored.QueryFailedRequests(FailureQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Failures) != len(after.Failures) {
		t.Fatalf("restore changed the list: %d rows became %d", len(before.Failures), len(after.Failures))
	}
	for index := range before.Failures {
		if before.Failures[index].Sequence != after.Failures[index].Sequence ||
			before.Failures[index].RequestID != after.Failures[index].RequestID {
			t.Fatalf("row %d differs: %#v vs %#v", index, before.Failures[index], after.Failures[index])
		}
	}
}
