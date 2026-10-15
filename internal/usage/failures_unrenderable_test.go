package usage

import (
	"strconv"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

// unrenderableFixture builds the request shape the shared fixture does not
// have: the upstream was reached and answered, and the request failed after
// that — outbound redaction refused the answer, or the renderer could not carry
// it. The attempt is settled as success before that verdict is reached, so the
// chain ends in a success while the request is finalized as provider_error.
//
// It is built on its own rather than added to failureFixture because the shared
// one is addressed by index and counted by several tests.
func unrenderableFixture(t *testing.T) *Aggregate {
	t.Helper()
	aggregate := newTestAggregate(t)
	at := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	var sequence uint64
	apply := func(event ledger.Event) {
		t.Helper()
		sequence++
		if err := aggregate.Apply(ledger.Record{
			Generation: 1, Sequence: sequence, Offset: int64(sequence * 100), Event: event,
		}); err != nil {
			t.Fatal(err)
		}
	}
	base := ledger.Event{
		RequestID: "req_unrenderable", ProjectID: "project", PeriodID: "period",
		RequestedModel: "chat", OccurredAt: at,
	}
	accepted := base
	accepted.EventID, accepted.Kind = "accepted", ledger.EventRequestAccepted
	apply(accepted)

	failed := base
	failed.EventID, failed.Kind = "att_1_settled", ledger.EventAttemptSettled
	failed.AttemptID, failed.AttemptNumber = "att_1", 1
	failed.ProviderID, failed.DeploymentID, failed.ProviderModel = "provider_a", "dep_a", "model-a"
	failed.Outcome, failed.ErrorClass, failed.HTTPStatus = "provider_error", "provider_5xx", 503
	failed.ProviderCode, failed.ProviderRequestID = "srv_overload", "upstream-req-first"
	failed.CommittedMicrosUSD = ledger.MicrosUSD(1)
	apply(failed)

	succeeded := base
	succeeded.EventID, succeeded.Kind = "att_2_settled", ledger.EventAttemptSettled
	succeeded.AttemptID, succeeded.AttemptNumber = "att_2", 2
	succeeded.ProviderID, succeeded.DeploymentID, succeeded.ProviderModel = "provider_b", "dep_b", "model-b"
	succeeded.Outcome = "success"
	succeeded.CommittedMicrosUSD = ledger.MicrosUSD(1)
	apply(succeeded)

	finalized := base
	finalized.EventID, finalized.Kind = "final", ledger.EventRequestFinalized
	finalized.OccurredAt, finalized.Outcome = at.Add(time.Second), "provider_error"
	apply(finalized)
	return aggregate
}

// The failed-request list used to walk back past the successful attempt and
// hand the operator the provider_request_id of a call that worked. They take it
// to the upstream and ask about a successful request — which is the same defect
// the terminal ERROR log was fixed for, on the other side of the same event
// stream. Nothing on this chain explains this request, so it carries no
// provider context at all, like a request that never reached an upstream.
func TestARequestThatFailedAfterTheUpstreamAnsweredCarriesNoProviderContext(t *testing.T) {
	page, err := unrenderableFixture(t).QueryFailedRequests(FailureQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Failures) != 1 {
		t.Fatalf("failures = %d, want 1", len(page.Failures))
	}
	failure := page.Failures[0]
	if failure.RequestID != "req_unrenderable" || failure.Outcome != "provider_error" {
		t.Fatalf("failure = %#v", failure)
	}
	// The request is still listed — it did fail, and the summary card counts it.
	if failure.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", failure.Attempts)
	}
	if failure.LastFailure != nil {
		t.Fatalf("the successful attempt's chain was given a provider context: %#v", failure.LastFailure)
	}
}

// The narrowing must not reach the ordinary case: a chain that ends in a
// failure is still explained by that failure.
func TestAChainThatEndsInFailureStillExplainsTheRequest(t *testing.T) {
	page, err := failureFixture(t).QueryFailedRequests(FailureQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, failure := range page.Failures {
		if failure.RequestID != "req_failed" {
			continue
		}
		found = true
		if failure.LastFailure == nil || failure.LastFailure.AttemptID != "att_f_2" {
			t.Fatalf("last failure = %#v", failure.LastFailure)
		}
	}
	if !found {
		t.Fatal("req_failed is missing from the failed-request list")
	}
}

// A page's cost has to be bounded whatever the history behind it. A filter
// that matches nothing would otherwise read every failed request ever stored
// to say so; with the scan budget it stops, returns what it found, and hands
// back a cursor to continue from.
func TestAFailurePageExaminesAtMostTheScanBudget(t *testing.T) {
	previous := historyScanBudget
	historyScanBudget = 10
	t.Cleanup(func() { historyScanBudget = previous })

	aggregate := newTestAggregate(t)
	at := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	var sequence uint64
	for index := range 25 {
		requestID := "req_" + strconv.Itoa(index)
		for _, event := range []ledger.Event{
			{EventID: requestID + "_settled", Kind: ledger.EventAttemptSettled, RequestID: requestID,
				AttemptID: requestID + "_att", AttemptNumber: 1, ProjectID: "project", PeriodID: "period",
				ProviderID: "provider_a", OccurredAt: at, Outcome: "provider_error", CommittedMicrosUSD: ledger.MicrosUSD(1)},
			{EventID: requestID + "_final", Kind: ledger.EventRequestFinalized, RequestID: requestID,
				ProjectID: "project", PeriodID: "period", OccurredAt: at, Outcome: "provider_error"},
		} {
			sequence++
			if err := aggregate.Apply(ledger.Record{Generation: 1, Sequence: sequence, Offset: int64(sequence * 100), Event: event}); err != nil {
				t.Fatal(err)
			}
		}
	}
	page, err := aggregate.QueryFailedRequests(FailureQuery{Limit: 5, ProviderID: "provider_nobody_used"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Failures) != 0 || page.NextCursor == "" {
		t.Fatalf("a page past the budget returned %d rows and cursor %q, want none and a cursor", len(page.Failures), page.NextCursor)
	}
	// The cursor resumes below the last examined row, so paging on walks the
	// rest of the history a budget at a time and then ends.
	pages := 1
	for cursor := page.NextCursor; cursor != ""; pages++ {
		before, err := DecodeCursor(cursor)
		if err != nil {
			t.Fatal(err)
		}
		next, err := aggregate.QueryFailedRequests(FailureQuery{Limit: 5, ProviderID: "provider_nobody_used", BeforeSequence: before})
		if err != nil {
			t.Fatal(err)
		}
		cursor = next.NextCursor
		if pages > 10 {
			t.Fatal("paging never reached the end of the history")
		}
	}
	if pages != 3 {
		t.Fatalf("25 failed requests at a budget of 10 took %d pages, want 3", pages)
	}
}
