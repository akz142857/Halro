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

	// One pass over the attempts, rather than a scan per candidate row: a page
	// of 100 requests over a long-running aggregate would otherwise walk the
	// whole attempt history a hundred times.
	attemptsByRequest := make(map[string][]int)
	for index, attempt := range a.attempts {
		attemptsByRequest[attempt.RequestID] = append(attemptsByRequest[attempt.RequestID], index)
	}

	page := FailurePage{Failures: make([]RequestFailure, 0, query.Limit+1)}
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
// one that decided the outcome. The successful attempts of a chain are not
// candidates: a request can only end unsuccessfully on a failure or on no
// attempt at all.
func (a *Aggregate) lastFailure(indexes []int) *FailureContext {
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
