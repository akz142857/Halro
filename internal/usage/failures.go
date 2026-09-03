package usage

import (
	"errors"
	"time"
)

// The failed-request view. It answers a different question from QueryAttempts,
// and the difference is the whole reason it exists: an attempt row is one call
// to an upstream, and a request that failed one and then succeeded on the next
// target is not a failed request. Counting attempt failures where the summary
// card counts request failures is how a console ends up with two numbers for
// one thing.
//
// The source is the same RequestFinalized event the card's `request_errors`
// comes from, so the list and the figure above it cannot disagree. That also
// fixes what the list can never contain: a failure that returned before
// beginRequestRun — an invalid key, an unrouted model, a rate-limit refusal —
// wrote no ledger request and is absent from both.

type FailureQuery struct {
	BeforeSequence uint64
	Limit          int
	ProjectID      string
	// RequestID answers the question this list is most often opened with: a
	// caller reports an ID from a failed call and wants to know what happened
	// to it. Matched exactly, like the attempt list's, so a truncated or
	// mistyped ID returns nothing rather than a plausible neighbour.
	RequestID string
	// ProviderID, DeploymentID and ProviderModel describe an attempt, not a
	// request. A request matches when any of its attempts does — which
	// necessarily excludes every request that failed before reaching an
	// upstream, because those have no attempts to match. That is the honest
	// answer to "show me this deployment's failures" and not a gap: a budget
	// refusal did not happen at a deployment.
	ProviderID     string
	DeploymentID   string
	ProviderModel  string
	RequestedModel string
	Start          time.Time
	End            time.Time
}

// FailureContext is the last failed attempt of a request, reduced to what
// explains it. It is absent — not blanked out — for a request that never made
// a failing upstream call, because rendering an empty provider context would
// report "the upstream did not answer" for a request that never asked.
type FailureContext struct {
	AttemptID     string `json:"attempt_id"`
	AttemptNumber int    `json:"attempt"`
	ErrorClass    string `json:"error_class,omitempty"`
	// The upstream's own status, under the name the log and the plan use for
	// it. The attempt row calls the same value http_status; here it is
	// qualified because a failed request also has a status of its own that the
	// caller saw, and the two are not the same number.
	ProviderStatus int    `json:"provider_status,omitempty"`
	ProviderID     string `json:"provider_id,omitempty"`
	DeploymentID   string `json:"deployment_id,omitempty"`
	ProviderModel  string `json:"provider_model,omitempty"`
	// What goes on a ticket to the upstream. Absent on an attempt recorded
	// before these were kept, and absent when the upstream named none — the
	// console distinguishes the two by the attempt's own age rather than by
	// filling either with a placeholder.
	ProviderCode      string    `json:"provider_code,omitempty"`
	ProviderRequestID string    `json:"provider_request_id,omitempty"`
	FailurePhase      string    `json:"failure_phase,omitempty"`
	CompletedAt       time.Time `json:"completed_at"`
}

type RequestFailure struct {
	RequestID      string `json:"request_id"`
	ProjectID      string `json:"project_id"`
	KeyID          string `json:"key_id,omitempty"`
	RequestedModel string `json:"requested_model,omitempty"`
	// Outcome is the ledger's own terminal state, not a rendered category.
	// Which of them count as a policy rejection is a question the console
	// answers for a reader; the record answers what happened.
	Outcome     string          `json:"outcome"`
	Sequence    uint64          `json:"sequence"`
	AcceptedAt  time.Time       `json:"accepted_at"`
	CompletedAt time.Time       `json:"completed_at"`
	Attempts    int64           `json:"attempts"`
	Fallbacks   int64           `json:"fallbacks"`
	LastFailure *FailureContext `json:"last_failure,omitempty"`
}

type FailurePage struct {
	Failures   []RequestFailure
	NextCursor string
}

// QueryFailedRequests pages the requests the ledger recorded as anything other
// than a success, newest first.
func (a *Aggregate) QueryFailedRequests(query FailureQuery) (FailurePage, error) {
	if query.Limit < 1 || query.Limit > 100 {
		return FailurePage{}, errors.New("usage page limit must be between 1 and 100")
	}
	a.mu.RLock()
	defer a.mu.RUnlock()

	// The rows this page can contain, chosen before any attempt is touched. The
	// attempt index is then built for those rows alone, which is what keeps a
	// page's cost proportional to a page: indexing the whole window built a map
	// with an entry per request held — tens of millions on a busy instance,
	// hundreds of megabytes, allocated per call and under the read lock the
	// usage collector needs — in order to return at most a hundred rows.
	//
	// A query that filters on attempt fields still needs every request's
	// attempts to decide which rows qualify, so it indexes the window. That
	// path is the console's advanced filter rather than its default view, and
	// one pass over the attempts is still cheaper there than a scan per row.
	candidates := a.failureCandidates(query)
	attemptsByRequest := a.indexAttempts(candidates, attemptFiltered(query))

	page := FailurePage{Failures: make([]RequestFailure, 0, query.Limit+1)}
	for _, summary := range candidates {
		indexes := attemptsByRequest[summary.RequestID]
		if !a.attemptsMatch(indexes, query) {
			continue
		}
		page.Failures = append(page.Failures, RequestFailure{
			RequestID: summary.RequestID, ProjectID: summary.ProjectID, KeyID: summary.KeyID,
			RequestedModel: summary.RequestedModel, Outcome: summary.Outcome,
			Sequence: summary.Sequence, AcceptedAt: summary.AcceptedAt,
			CompletedAt: summary.CompletedAt, Attempts: summary.Attempts,
			Fallbacks: summary.Fallbacks, LastFailure: a.lastFailure(indexes),
		})
		if len(page.Failures) == query.Limit+1 {
			page.Failures = page.Failures[:query.Limit]
			page.NextCursor = EncodeCursor(page.Failures[len(page.Failures)-1].Sequence)
			break
		}
	}
	return page, nil
}

// failureCandidates walks the summaries newest first and keeps the rows a page
// could contain, applying every filter that does not need an attempt.
//
// Without an attempt filter the walk stops at one page: nothing further can
// appear in the result. With one it collects the whole matching window, because
// any of those rows may be the one that survives.
func (a *Aggregate) failureCandidates(query FailureQuery) []RequestSummary {
	filtered := attemptFiltered(query)
	candidates := make([]RequestSummary, 0, query.Limit+1)
	for index := len(a.summaries) - 1; index >= 0; index-- {
		summary := a.summaries[index]
		if summary.Outcome == "" || summary.Outcome == "success" {
			continue
		}
		if query.BeforeSequence > 0 && summary.Sequence >= query.BeforeSequence {
			continue
		}
		if query.ProjectID != "" && summary.ProjectID != query.ProjectID ||
			query.RequestID != "" && summary.RequestID != query.RequestID ||
			query.RequestedModel != "" && summary.RequestedModel != query.RequestedModel ||
			!query.Start.IsZero() && summary.CompletedAt.Before(query.Start) ||
			!query.End.IsZero() && !summary.CompletedAt.Before(query.End) {
			continue
		}
		candidates = append(candidates, summary)
		if !filtered && len(candidates) == query.Limit+1 {
			break
		}
	}
	return candidates
}

// indexAttempts maps request to attempt positions for the rows that need it.
func (a *Aggregate) indexAttempts(candidates []RequestSummary, filtered bool) map[string][]int {
	wanted := make(map[string]struct{}, len(candidates))
	for _, summary := range candidates {
		wanted[summary.RequestID] = struct{}{}
	}
	indexed := make(map[string][]int, len(wanted))
	for index, attempt := range a.attempts {
		if !filtered {
			if _, needed := wanted[attempt.RequestID]; !needed {
				continue
			}
		}
		indexed[attempt.RequestID] = append(indexed[attempt.RequestID], index)
	}
	return indexed
}

func attemptFiltered(query FailureQuery) bool {
	return query.ProviderID != "" || query.DeploymentID != "" || query.ProviderModel != ""
}

// attemptsMatch applies the attempt-scoped filters. A query that names none of
// them keeps every request, including the ones with no attempts at all — which
// is what makes the unfiltered list add up to the summary card's count.
func (a *Aggregate) attemptsMatch(indexes []int, query FailureQuery) bool {
	if query.ProviderID == "" && query.DeploymentID == "" && query.ProviderModel == "" {
		return true
	}
	for _, index := range indexes {
		attempt := a.attempts[index]
		if query.ProviderID != "" && attempt.ProviderID != query.ProviderID ||
			query.DeploymentID != "" && attempt.DeploymentID != query.DeploymentID ||
			query.ProviderModel != "" && attempt.ProviderModel != query.ProviderModel {
			continue
		}
		return true
	}
	return false
}

// lastFailure is the final unsuccessful attempt of the request, which is the
// one that decided the outcome — when there is one.
//
// A request can fail after its last attempt succeeded: the upstream answered,
// and the answer could not be put on the wire (outbound redaction refused it,
// or the renderer could not carry it). The attempt is settled as success before
// that verdict is reached, so the chain ends in a success while the request is
// finalized as provider_error.
//
// Nothing on the chain explains that request. Walking back to an earlier failed
// attempt hands the operator the provider_request_id of a call that worked, and
// they take it to the upstream and ask about a successful request — which is the
// same defect the terminal ERROR log was fixed for, on the other side of the
// same event stream. So a chain that ends in success carries no provider
// context at all, exactly like a request that never reached an upstream.
func (a *Aggregate) lastFailure(indexes []int) *FailureContext {
	if len(indexes) > 0 && a.attempts[indexes[len(indexes)-1]].Status == "success" {
		return nil
	}
	for position := len(indexes) - 1; position >= 0; position-- {
		attempt := a.attempts[indexes[position]]
		if attempt.Status == "success" {
			continue
		}
		return &FailureContext{
			AttemptID: attempt.AttemptID, AttemptNumber: attempt.AttemptNumber,
			ErrorClass: attempt.ErrorClass, ProviderStatus: attempt.HTTPStatus,
			ProviderID: attempt.ProviderID, DeploymentID: attempt.DeploymentID,
			ProviderModel: attempt.ProviderModel,
			ProviderCode:  attempt.ProviderCode, ProviderRequestID: attempt.ProviderRequestID,
			FailurePhase: attempt.FailurePhase, CompletedAt: attempt.CompletedAt,
		}
	}
	return nil
}
