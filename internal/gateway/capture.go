package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/failurecapture"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

// FailureCapture is the store a failed request's payload is written to. Nil
// disables capture entirely, which is the default and what every test that does
// not name it gets.
type FailureCapture interface {
	PutContext(context.Context, failurecapture.Record) (bool, error)
	Saturated() bool
}

const failureCaptureQueueCapacity = 64

func (s *Service) startFailureCapture() {
	if s == nil || s.failureCapture == nil {
		return
	}
	s.captureMu.Lock()
	defer s.captureMu.Unlock()
	if s.captureQueue != nil || s.captureClosed {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.captureQueue = make(chan failurecapture.Record, failureCaptureQueueCapacity)
	s.captureCancel = cancel
	s.captureDone = make(chan struct{})
	go s.runFailureCapture(ctx, s.captureQueue, s.captureDone)
}

func (s *Service) runFailureCapture(ctx context.Context, queue <-chan failurecapture.Record, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case record, ok := <-queue:
			if !ok {
				return
			}
			s.storeCapture(ctx, record)
			if ctx.Err() != nil {
				return
			}
		}
	}
}

func (s *Service) storeCapture(ctx context.Context, record failurecapture.Record) {
	written, err := s.failureCapture.PutContext(ctx, record)
	switch {
	case err != nil && !errors.Is(err, context.Canceled) && s.captureDegraded.CompareAndSwap(false, true):
		// Named without payload or upstream prose: the material that failed to
		// be stored must not escape into the diagnostic about that failure.
		s.logger.Warn("failure capture was not stored", "request_id", record.RequestID, "error", err)
	case !written && s.failureCapture.Saturated():
		s.logger.Warn("failure capture stopped for the day at its record ceiling", "request_id", record.RequestID)
	}
}

func (s *Service) enqueueCapture(record failurecapture.Record) {
	// Tests that install a capture after construction retain the direct behavior.
	// A normally configured service is marked async by its constructor and starts
	// its queue atomically before accepting the first record.
	s.captureMu.Lock()
	if s.captureQueue == nil && s.captureAsync && !s.captureClosed {
		ctx, cancel := context.WithCancel(context.Background())
		s.captureQueue = make(chan failurecapture.Record, failureCaptureQueueCapacity)
		s.captureCancel = cancel
		s.captureDone = make(chan struct{})
		go s.runFailureCapture(ctx, s.captureQueue, s.captureDone)
	}
	if s.captureQueue == nil {
		closed := s.captureClosed
		s.captureMu.Unlock()
		if !closed {
			s.storeCapture(context.Background(), record)
		}
		return
	}
	if s.captureClosed {
		s.captureMu.Unlock()
		return
	}
	select {
	case s.captureQueue <- record:
		s.captureMu.Unlock()
	default:
		s.captureMu.Unlock()
		if s.captureDegraded.CompareAndSwap(false, true) {
			s.logger.Warn("failure capture queue is full; payload was dropped", "request_id", record.RequestID)
		}
	}
}

// ShutdownFailureCapture stops accepting new diagnostics and drains all queued
// captures before their vault and data directory are closed. The caller owns
// the deadline; cancellation is passed into the active write.
func (s *Service) ShutdownFailureCapture(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.captureCloseOnce.Do(func() {
		s.captureMu.Lock()
		s.captureClosed = true
		if s.captureQueue != nil {
			close(s.captureQueue)
		}
		s.captureMu.Unlock()
	})

	s.captureMu.Lock()
	done, cancel := s.captureDone, s.captureCancel
	s.captureMu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		if cancel != nil {
			cancel()
		}
		return nil
	case <-ctx.Done():
		if cancel != nil {
			cancel()
		}
		// FailureCapture.PutContext is a cancellation contract: once cancelled,
		// the worker must release references to the vault before Runtime closes
		// it. The concrete store can abandon a stalled regular-file syscall while
		// its one bounded writer finishes with already sealed bytes.
		<-done
		return fmt.Errorf("drain failure captures: %w", ctx.Err())
	}
}

// capturedOutcomes are the terminal states whose payload can explain the
// failure. It is deliberately a third, narrower set than "failed request" and
// than "earns an ERROR record", because the question each set answers is
// different and collapsing them would be wrong in both directions.
//
//   - provider_error: the upstream refused or failed, or its answer could not be
//     rendered. The request and Halro's structured classification diagnose it
//     without retaining the upstream's untrusted prose.
//   - unsupported_feature: the target could not serve the shape of the request.
//     Which field, and what was in it, is the only way to see why.
//
// The rule is not "it reached an upstream" — unsupported_feature never does.
// It is that a payload explains the failure *and* a client cannot produce it at
// will. Everything else is excluded because it fails one half or the other:
//
//   - policy_rejected is redaction refusing an answer. Storing the content a
//     policy just refused is the one thing this store must never do — it would
//     make the capture the leak the policy exists to prevent.
//   - rejected and token_guard_rejected are budget, breaker, concurrency and
//     cost ceilings. There is nothing to reproduce, and a client in a retry
//     loop produces them at its own rate, which is how a bounded store fills in
//     minutes.
//   - accounting_error is the ledger being unavailable. The payload says nothing
//     about that, and keeping caller material to explain a disk problem is a
//     trade nobody would make deliberately.
//
// unsupported_feature is the one that has to be argued rather than read off the
// rule. It is reached only by abort, after the capability filters have already
// passed the target — so it is an internal disagreement between routing and
// dispatch rather than something a caller can ask for, and the request is
// exactly what shows which field was refused. If a route is ever found that
// lets a client drive it, it belongs with the two above.
var capturedOutcomes = map[string]struct{}{
	"provider_error":      {},
	"unsupported_feature": {},
}

func capturesPayload(outcome string) bool {
	_, captured := capturedOutcomes[outcome]
	return captured
}

// captureGatewayRequest remembers the decoded body at Halro's public API
// boundary. Keeping it beside the normalized request is what lets an operator
// distinguish max_tokens from max_completion_tokens (and equivalent facade
// differences) after translation has changed the request's shape.
func (run *requestRun) captureGatewayRequest(request any) {
	if run == nil || run.service.failureCapture == nil {
		return
	}
	run.capturedGatewayRequest = request
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

// captureResponse remembers only the safe shape of a semantic answer Halro
// could not put on the wire. Provider-written values are excluded even on a
// successful response because the upstream saw the credential and can echo it.
func (run *requestRun) captureResponse(response any) {
	if run == nil || run.service.failureCapture == nil {
		return
	}
	result, ok := response.(semantic.GenerateResult)
	if !ok {
		return
	}
	shape := safeGenerateResponseShape{
		Choices: len(result.Choices), HasUsage: result.Usage != nil,
		Translation: string(result.Translation), MappingRevision: result.MappingRevision,
	}
	for _, choice := range result.Choices {
		for _, content := range choice.Message.Content {
			shape.ContentKinds = append(shape.ContentKinds, string(content.Kind))
		}
	}
	run.capturedResponse = shape
}

// safeGenerateResponseShape keeps what explains a render mismatch without any
// provider-written text, URLs, tool arguments or identifiers. A successful
// upstream is still untrusted and could echo its Authorization header in model
// output just as easily as in an error body.
type safeGenerateResponseShape struct {
	Choices         int      `json:"choices"`
	ContentKinds    []string `json:"content_kinds,omitempty"`
	HasUsage        bool     `json:"has_usage"`
	Translation     string   `json:"translation,omitempty"`
	MappingRevision uint64   `json:"mapping_revision,omitempty"`
}

// captureProviderFailure records the upstream's own answer to a failed attempt.
//
// Provider prose is deliberately not retained. An upstream is allowed to quote
// the credential it received in its error body, and encryption plus audited
// reads do not make that credential appropriate for a read-only administrator.
// The structured status and class below remain useful without copying any
// upstream-controlled sentence across that boundary.
func (run *requestRun) captureProviderFailure(providerErr error) {
	if run == nil || run.service.failureCapture == nil || providerErr == nil {
		return
	}
	var classified *provider.Error
	if !errors.As(providerErr, &classified) {
		return
	}
	run.capturedResponse = upstreamFailureBody{
		Status: classified.StatusCode,
		Class:  string(classified.Class),
	}
}

// upstreamFailureBody is the safe shape a captured upstream refusal takes.
type upstreamFailureBody struct {
	Status int    `json:"provider_status,omitempty"`
	Class  string `json:"error_class,omitempty"`
}

// writeCapture stores the payload of a request that failed. It runs on the same
// finalize boundary as the terminal log record, and it is best-effort in the
// strongest sense: nothing it can do changes what the caller is told, and a
// store that cannot be written drops the capture rather than failing a request
// that has already failed.
func (run *requestRun) writeCapture(outcome string) {
	store := run.service.failureCapture
	// Same exclusion as the terminal record, and for the same reason: a wave of
	// client disconnects would otherwise store one prompt per cancelled request
	// and burn the day's ceiling before the real incident starts.
	if store == nil || !capturesPayload(outcome) || run.callerAbandoned() {
		return
	}
	if run.capturedGatewayRequest == nil && run.capturedRequest == nil && run.capturedResponse == nil {
		return
	}
	descriptor := run.terminalDescriptor(outcome)
	record := failurecapture.Record{
		RequestID:                run.requestID,
		ProjectID:                run.principal.Project.ID,
		Outcome:                  outcome,
		OfferingID:               run.lastTarget.OfferingID,
		ProfileID:                run.lastTarget.ProfileID,
		AccountRegionID:          run.lastTarget.AccountRegionID,
		ProviderCode:             descriptor.ProviderCode,
		ProviderFailureReason:    descriptor.ProviderFailureReason,
		ProviderRequestID:        descriptor.ProviderRequestID,
		FailurePhase:             descriptor.Phase,
		Retryable:                descriptor.Retryable,
		Ambiguous:                descriptor.Ambiguous,
		FailureSemanticsRecorded: descriptor.FailureSemanticsRecorded,
		GatewayRequest:           encodeCaptured(run.capturedGatewayRequest),
		Request:                  encodeCaptured(run.capturedRequest),
		Response:                 encodeCaptured(run.capturedResponse),
	}
	run.service.enqueueCapture(record)
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
