package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/provider"
)

// The failure phase: where along the request the thing that ended it happened.
// It is the first question an operator asks and the one a status code answers
// worst — a 502 is written by the provider path, by a render that could not
// carry the answer, and by an accounting refusal, and each sends them somewhere
// different.
// outcomeSuccess is the one terminal state that is not a failure. It is named
// rather than spelled out at each comparison because three separate decisions
// now turn on it — whether an attempt records an HTTP 200, whether a request
// earns a terminal ERROR, and whether its payload is kept.
const outcomeSuccess = "success"

const (
	// phasePreProvider covers everything after admission and before an upstream
	// call: a target refused for capability, a Token Guard recheck, a budget or
	// concurrency rejection.
	phasePreProvider = "pre_provider"
	phaseProvider    = "provider"
	// phaseResponseRender is an upstream that answered and an answer Halro
	// could not put on the caller's wire.
	phaseResponseRender = "response_render"
	phaseAccounting     = "accounting"
	phaseClient         = "client"
)

// FailureDescriptor is what may be said about a failure, as a fixed set of
// fields rather than as whatever the error happened to be carrying.
//
// It is deliberately not a wrapper around a provider error. The upstream's own
// sentence is a response body — the one thing an upstream is most likely to
// quote back is the credential it just refused — so nothing here is free text
// taken from one. Every field is either produced by Halro, an enumerated class,
// a status code, or an identifier that has been through
// provider.SafeProviderIdentifier.
type FailureDescriptor struct {
	Phase string
	// Class is always a member of provider.ErrorClass. An unclassified error
	// resolves to ErrorUnknown rather than to a literal of its own: a log whose
	// error_class is sometimes outside the enum cannot be aggregated by it, and
	// that is exactly what the console's dictionary and every alert rule key on.
	Class             provider.ErrorClass
	ProviderStatus    int
	ProviderCode      string
	ProviderRequestID string
	// ErrorType is the Go type of an error nothing classified. A type name is
	// produced by the code rather than by an upstream, and it answers the
	// question this case is actually asked: which component produced a failure
	// the adapter contract should have classified. The error's text stays out,
	// because from here an adapter that ignored the contract and Halro's own
	// internal error are indistinguishable, and one of them can hold a body.
	ErrorType string
	Retryable bool
	Ambiguous bool
	// Where it happened, carried from the target the attempt ran against.
	PublicModel  string
	DeploymentID string
	ProviderID   string
	BindingID    string
}

// describeProviderFailure reduces whatever ended an attempt to what may be
// logged about it. It is the single place that classification happens, so the
// attempt's WARN record and the request's terminal ERROR cannot disagree about
// what went wrong.
func describeProviderFailure(providerErr error, target provider.Target) FailureDescriptor {
	descriptor := FailureDescriptor{
		Phase:        phaseProvider,
		PublicModel:  target.PublicModel,
		DeploymentID: target.DeploymentID,
		ProviderID:   target.ProviderID,
		BindingID:    target.BindingID,
	}
	var classified *provider.Error
	switch {
	case errors.As(providerErr, &classified):
		descriptor.Class = classified.Class
		descriptor.Retryable = classified.Retryable
		descriptor.Ambiguous = classified.Ambiguous
		descriptor.ProviderStatus = classified.StatusCode
		// Both identifiers are narrowed here as well as at the adapters that
		// narrow them, which is not belt and braces: this is the line the
		// invariant binds to. An adapter is where the value is understood, but
		// the rule — no provider response bytes in a log, an error, a metric or
		// an audit record — is about where it is written, and an adapter added
		// later that forgets to narrow must not be able to widen this.
		descriptor.ProviderCode = provider.SafeProviderIdentifier(classified.ProviderCode)
		descriptor.ProviderRequestID = provider.SafeProviderIdentifier(classified.ProviderRequestID)
	case errors.Is(providerErr, context.Canceled), errors.Is(providerErr, context.DeadlineExceeded):
		// The caller went away, or the deadline did. This used to be written as
		// a literal `client_disconnected_or_timed_out`, which was not a member
		// of provider.ErrorClass — so a value that only this branch produced
		// appeared in the log beside eight that could be aggregated, and the
		// console had nothing to translate it with. provider.TransportClass
		// already draws the same distinction for the transport, so the two
		// paths now answer identically.
		descriptor.Class = provider.TransportClass(providerErr)
		descriptor.Phase = phaseClient
	default:
		descriptor.Class = provider.ErrorUnknown
		descriptor.ErrorType = fmt.Sprintf("%T", providerErr)
	}
	return descriptor
}

// attributes renders the descriptor for a log record, omitting what it does not
// have rather than writing an empty value: a `provider_status` of 0 reads as an
// upstream that answered with 0, and a blank `provider_code` reads as an
// upstream that named no code when in fact none was ever asked for.
func (d FailureDescriptor) attributes() []any {
	attributes := []any{"phase", d.Phase, "error_class", string(d.Class)}
	if d.ProviderStatus > 0 {
		attributes = append(attributes, "provider_status", d.ProviderStatus)
	}
	if d.ProviderCode != "" {
		attributes = append(attributes, "provider_code", d.ProviderCode)
	}
	if d.ProviderRequestID != "" {
		attributes = append(attributes, "provider_request_id", d.ProviderRequestID)
	}
	if d.ErrorType != "" {
		attributes = append(attributes, "error_type", d.ErrorType)
	}
	if d.PublicModel != "" {
		attributes = append(attributes, "public_model", d.PublicModel)
	}
	if d.DeploymentID != "" {
		attributes = append(attributes, "deployment_id", d.DeploymentID)
	}
	if d.ProviderID != "" {
		attributes = append(attributes, "provider_id", d.ProviderID)
	}
	if d.BindingID != "" {
		attributes = append(attributes, "binding_id", d.BindingID)
	}
	return attributes
}

// writesFailureError decides which terminal states earn a request-level ERROR.
//
// The line is not "the request did not succeed". Four of the six non-success
// outcomes are a policy doing its job — a spent budget, an open breaker, a full
// target, a Token Guard ceiling, a capability the target does not serve, a
// redaction rule that refused an answer — and a client in a retry loop produces
// them at its own rate, not at the rate things break. Writing those would fill
// a bounded error file in minutes and push the incident's first real error out
// of it, exactly when it is wanted. They are counted, listed in the console and
// explained by the rate-limit and breaker figures; they are not incidents.
//
// The two that are written are the two nothing outside Halro can drive: an
// upstream that failed the request outright, and accounting that could not be
// asked. Both are conditions an operator is expected to act on.
//
// The consequence has to be stated wherever the two numbers are read together:
// the ERROR count is a subset of the failed-request count, never equal to it,
// and neither can be used to check the other.
// callerAbandoned reports that the caller hung up rather than that anything
// broke.
//
// A cancelled request finalizes as provider_error, because that is what the
// ledger has to record for an attempt that was cut short. But the rule the two
// ERROR-writing outcomes are chosen by is "nothing outside Halro can drive
// this", and a client hanging up is driven entirely from outside: a frontend
// deploy or a gateway restart cancels every request in flight at once. Writing
// those would fill a bounded error file in seconds and push the incident's
// first real error out of it — the exact failure mode the four policy outcomes
// are excluded to prevent. reportBreaker already refuses to judge a target on
// this signal, for the same reason.
//
// A deadline that expired is not this: nobody hung up, something was too slow,
// and that is worth a record.
func (run *requestRun) callerAbandoned() bool {
	return run.failure.Class == provider.ErrorCanceled
}

func writesFailureError(outcome string) bool {
	switch outcome {
	case "provider_error", "accounting_error":
		return true
	default:
		return false
	}
}

// logFinalFailure writes the one terminal record for a request that failed.
//
// It hangs off finalize rather than off the return statements that produce the
// answer, for the same reason settlement does: there are more than a dozen exit
// paths across five protocol faces, finalize is the boundary every one of them
// crosses exactly once, and a record written at the returns would be missing
// from whichever ones were added later.
//
// The caller's HTTP status is deliberately not here. On most paths it is chosen
// after finalize has run — exhaustedAttemptsError maps the last error into a
// response once the accounting is closed — so a status on this record would
// either be a guess or would move the mapping earlier to satisfy a log. The
// Request ID is what joins this record to the response the caller saw.
func (run *requestRun) logFinalFailure(outcome string, accountingRecorded bool) {
	if !writesFailureError(outcome) || run.callerAbandoned() {
		return
	}
	attributes := []any{"request_id", run.requestID, "outcome", outcome}
	attributes = append(attributes, run.terminalDescriptor(outcome).attributes()...)
	attributes = append(attributes,
		"attempts", run.attemptCount,
		"fallbacks", run.fallbackCount,
		"latency_millis", run.service.now().Sub(run.acceptedAt).Milliseconds(),
		// Whether the ledger took the terminal record. False means the console
		// will not be able to show this request at all, so the log is the only
		// account of it that exists — which is worth saying on the record
		// rather than leaving an operator to discover it by not finding the row.
		"accounting_recorded", accountingRecorded,
	)
	run.service.logger.Error("request failed", attributes...)
}

// terminalDescriptor picks what the terminal record reports.
//
// The last unresolved attempt failure explains a provider_error, and only that.
// Selecting on "is there a descriptor" instead was wrong in two directions at
// once: nothing cleared the descriptor when a later attempt succeeded, so a
// render failure after a fallback reported the earlier target's status and
// upstream request ID; and an accounting failure that happened to follow a
// failed attempt was written into the errors-only file as a provider 5xx,
// sending an operator upstream over a disk that could not be written.
func (run *requestRun) terminalDescriptor(outcome string) FailureDescriptor {
	if outcome == "provider_error" && run.failure.Class != "" {
		return run.failure
	}
	// No upstream failure explains this one. Naming a phase without inventing
	// an error class is the honest record — borrowing `unknown` would report an
	// unclassified upstream failure for a request whose upstream may have
	// answered perfectly well.
	descriptor := FailureDescriptor{Phase: phaseFor(outcome), Class: provider.ErrorUnknown}
	if run.lastTarget.DeploymentID != "" {
		descriptor.PublicModel = run.lastTarget.PublicModel
		descriptor.DeploymentID = run.lastTarget.DeploymentID
		descriptor.ProviderID = run.lastTarget.ProviderID
		descriptor.BindingID = run.lastTarget.BindingID
	}
	return descriptor
}

// phaseFor names where a failure with no attempt of its own happened. A
// provider_error that never produced a classified attempt failure is a render
// that could not carry the upstream's answer; everything else here is Halro's
// own accounting.
func phaseFor(outcome string) string {
	if outcome == "provider_error" {
		return phaseResponseRender
	}
	return phaseAccounting
}
