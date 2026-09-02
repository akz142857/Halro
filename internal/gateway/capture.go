package gateway

import (
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/failurecapture"
	"github.com/akz142857/Halro/internal/provider"
)

// FailureCapture is the store a failed request's payload is written to. Nil
// disables capture entirely, which is the default and what every test that does
// not name it gets.
type FailureCapture interface {
	Put(record failurecapture.Record) (bool, error)
	Saturated() bool
}

// capturedOutcomes are the terminal states whose payload can explain the
// failure. It is deliberately a third, narrower set than "failed request" and
// than "earns an ERROR record", because the question each set answers is
// different and collapsing them would be wrong in both directions.
//
//   - provider_error: the upstream refused or failed, or its answer could not be
//     rendered. The request and the upstream's reply are the whole diagnosis.
//   - unsupported_feature: the target could not serve the shape of the request.
//     Which field, and what was in it, is the only way to see why.
//
// Everything else is excluded on purpose:
//
//   - policy_rejected is redaction refusing an answer. Storing the content a
//     policy just refused is the one thing this store must never do — it would
//     make the capture the leak the policy exists to prevent.
//   - rejected and token_guard_rejected never reached an upstream. There is
//     nothing to reproduce, and they are produced at a runaway client's own
//     rate, which is how a bounded store fills in minutes.
//   - accounting_error is the ledger being unavailable. The payload says nothing
//     about that, and keeping caller material to explain a disk problem is a
//     trade nobody would make deliberately.
var capturedOutcomes = map[string]struct{}{
	"provider_error":      {},
	"unsupported_feature": {},
}

func capturesPayload(outcome string) bool {
	_, captured := capturedOutcomes[outcome]
	return captured
}

// captureRequest remembers the operation as it will go upstream, so a failure
// can be explained without buffering anything on the successful path.
//
// It holds a reference, not a copy, and serializes nothing until the request
// has actually failed. The value is alive for the duration of the call either
// way, so a successful request pays for this with one pointer assignment.
func (run *requestRun) captureRequest(request any) {
	if run == nil || run.service.failureCapture == nil {
		return
	}
	run.capturedRequest = request
}

// captureResponse remembers what came back, for the failures where something
// did. A render failure has a semantic answer Halro could not put on the wire;
// an upstream refusal has the body it refused with.
func (run *requestRun) captureResponse(response any) {
	if run == nil || run.service.failureCapture == nil {
		return
	}
	run.capturedResponse = response
}

// captureProviderFailure records the upstream's own answer to a failed attempt.
//
// This is the one piece of provider prose Halro keeps anywhere, and it is kept
// here rather than in the log for exactly the reason the log refuses it: the
// body may quote back the credential the upstream just rejected. In this store
// it is encrypted, bound to its request, bounded, expiring, and readable only
// through an audited action; in a log file it would be none of those.
func (run *requestRun) captureProviderFailure(providerErr error) {
	if run == nil || run.service.failureCapture == nil || providerErr == nil {
		return
	}
	var classified *provider.Error
	if !errors.As(providerErr, &classified) {
		return
	}
	body := providerFailureReason(classified)
	if body == "" {
		return
	}
	run.capturedResponse = upstreamFailureBody{
		Status:  classified.StatusCode,
		Class:   string(classified.Class),
		Message: body,
	}
}

// upstreamFailureBody is the shape a captured upstream refusal is stored in. It
// keeps the sentence apart from the status and class rather than as one blob,
// so a reader can tell what the adapter concluded from what the upstream said.
type upstreamFailureBody struct {
	Status  int    `json:"provider_status,omitempty"`
	Class   string `json:"error_class,omitempty"`
	Message string `json:"body"`
}

// writeCapture stores the payload of a request that failed. It runs on the same
// finalize boundary as the terminal log record, and it is best-effort in the
// strongest sense: nothing it can do changes what the caller is told, and a
// store that cannot be written drops the capture rather than failing a request
// that has already failed.
func (run *requestRun) writeCapture(outcome string) {
	store := run.service.failureCapture
	if store == nil || !capturesPayload(outcome) {
		return
	}
	if run.capturedRequest == nil && run.capturedResponse == nil {
		return
	}
	record := failurecapture.Record{
		RequestID: run.requestID,
		ProjectID: run.principal.Project.ID,
		Outcome:   outcome,
		Request:   encodeCaptured(run.capturedRequest),
		Response:  encodeCaptured(run.capturedResponse),
	}
	written, err := store.Put(record)
	switch {
	case err != nil:
		// Named without the payload: the thing that failed to be written must
		// not be written into the log instead.
		run.service.logger.Warn("failure capture was not stored",
			"request_id", run.requestID, "error", err)
	case !written && store.Saturated():
		run.service.logger.Warn("failure capture stopped for the day at its record ceiling",
			"request_id", run.requestID)
	}
}

// encodeCaptured serializes one side, or returns nothing when there is nothing
// to say. A value that will not marshal is dropped rather than replaced by an
// error string: this record is read as evidence, and an error message sitting
// where a request body should be is worse than an absent field.
func encodeCaptured(value any) json.RawMessage {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return encoded
}
