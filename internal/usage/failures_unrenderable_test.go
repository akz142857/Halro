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
	aggregate := NewAggregate()
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

// A page's cost has to be proportional to a page. Indexing the whole window
// built a map with an entry per request held — tens of millions on a busy
// instance — allocated per call, under the read lock the usage collector needs,
// in order to return at most a hundred rows.
func TestTheFailureIndexIsBuiltForThePageNotTheWindow(t *testing.T) {
	aggregate := NewAggregate()
	for index := range 500 {
		aggregate.attempts = append(aggregate.attempts, AttemptEvent{
			RequestID: "req_" + strconv.Itoa(index), AttemptID: "att", Status: "provider_error",
		})
	}
	candidates := []RequestSummary{{RequestID: "req_1"}, {RequestID: "req_2"}}

	indexed := aggregate.indexAttempts(candidates, false)
	if len(indexed) != len(candidates) {
		t.Fatalf("indexed %d requests for a %d-row page", len(indexed), len(candidates))
	}

	// The advanced filter still needs every request's attempts to decide which
	// rows qualify, and says so by asking for them.
	if all := aggregate.indexAttempts(candidates, true); len(all) != 500 {
		t.Fatalf("an attempt-filtered query indexed %d requests, want the window", len(all))
	}
}
