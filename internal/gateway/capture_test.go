package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/failurecapture"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/requestmeta"
	"github.com/akz142857/Halro/internal/semantic"
)

// recordingCapture stands in for the store so these tests are about what the
// gateway decides to capture, not about how it is sealed on disk.
type recordingCapture struct {
	records   []failurecapture.Record
	saturated bool
	err       error
}

func (c *recordingCapture) Put(record failurecapture.Record) (bool, error) {
	return c.PutContext(context.Background(), record)
}

func (c *recordingCapture) PutContext(_ context.Context, record failurecapture.Record) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	if c.saturated {
		return false, nil
	}
	c.records = append(c.records, record)
	return true, nil
}

func (c *recordingCapture) Saturated() bool { return c.saturated }

func withCapture(t *testing.T, f *fixture) *recordingCapture {
	t.Helper()
	capture := &recordingCapture{}
	f.service.failureCapture = capture
	return capture
}

// The whole point: a failed call can be reproduced. The request that went
// upstream and the answer that came back are both kept, under the request the
// caller was given.
func TestAFailedRequestCapturesWhatItSentAndWhatCameBack(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	capture := withCapture(t, &f)
	f.adapter.err = &provider.Error{
		Class: provider.ErrorBadRequest, Retryable: false, StatusCode: 400,
		ProviderCode: "invalid_image_url",
		Message:      "provider error (400): Error while downloading https://example.test/photo.png",
	}

	inbound := chatRequest()
	maxTokens := int64(8)
	inbound.MaxTokens = &maxTokens
	ctx := requestmeta.WithInboundRequest(context.Background(), inbound)
	if _, err := f.service.Chat(ctx, f.plaintext, inbound); err == nil {
		t.Fatal("the provider failure did not reach the caller")
	}
	if len(capture.records) != 1 {
		t.Fatalf("got %d captures, want 1", len(capture.records))
	}
	record := capture.records[0]
	if record.Outcome != "provider_error" || record.ProjectID != "project_1" || record.RequestID == "" {
		t.Fatalf("record = %#v", record)
	}
	if record.OfferingID != domain.OfferingOpenAIAPI || record.ProfileID != domain.ProfileOpenAIChatEmbeddings {
		t.Fatalf("capture lost provider product attribution: %#v", record)
	}
	// The public Gateway shape stays distinct from the normalized operation.
	// In particular, this tells the operator that the caller chose max_tokens,
	// which is the field the upstream refusal names.
	if !strings.Contains(string(record.GatewayRequest), `"max_tokens":8`) ||
		strings.Contains(string(record.GatewayRequest), "completion_token_limit") {
		t.Fatalf("the captured Gateway request lost its public field names: %s", record.GatewayRequest)
	}
	// The request as it went upstream, so the failure can be replayed.
	if !strings.Contains(string(record.Request), "hello") {
		t.Fatalf("the captured request does not hold what was sent: %s", record.Request)
	}
	// The upstream's sentence is not retained: an error body can quote the
	// credential it just rejected, and read-only administrators must never gain
	// that credential through diagnostics.
	var response map[string]any
	if err := json.Unmarshal(record.Response, &response); err != nil {
		t.Fatalf("captured response is not decodable: %s", record.Response)
	}
	if _, retained := response["body"]; retained || response["provider_status"] != float64(400) ||
		response["error_class"] != string(provider.ErrorBadRequest) {
		t.Fatalf("the capture did not preserve only structured diagnostics: %v", response)
	}
}

func TestProviderProseDoesNotEscapeThroughTheGatewayError(t *testing.T) {
	const canary = "opaque-authorizer-canary-value"
	f := newFixture(t, 1_000_000)
	defer f.close()
	f.adapter.err = &provider.Error{
		Class: provider.ErrorAuthentication, StatusCode: 401,
		Message: "upstream echoed Bearer " + canary,
	}
	_, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
	if err == nil {
		t.Fatal("the provider failure did not reach the caller")
	}
	if strings.Contains(fmt.Sprintf("%+v", err), canary) {
		t.Fatal("provider prose escaped through the gateway error chain")
	}
}

// The successful path stores nothing. That is what keeps this a small tail of
// traffic rather than a copy of it, and it is the property an operator is
// trusting when they turn the feature on.
func TestASuccessfulRequestCapturesNothing(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	capture := withCapture(t, &f)

	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err != nil {
		t.Fatal(err)
	}
	if len(capture.records) != 0 {
		t.Fatalf("a successful call was captured: %#v", capture.records)
	}
}

// A request that fell back and succeeded is a success, here as everywhere else.
// Capturing it would store the payload of a call the caller was served.
func TestAFallbackThatSucceedsCapturesNothing(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	capture := withCapture(t, &f)
	f.adapter.err = &provider.Error{
		Class: provider.ErrorProvider5xx, Retryable: true, StatusCode: 503, Message: "unavailable",
	}
	registerFallback(t, &f, &fakeAdapter{response: f.adapter.response})

	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err != nil {
		t.Fatalf("the fallback did not answer: %v", err)
	}
	if len(capture.records) != 0 {
		t.Fatalf("a request that succeeded on fallback was captured: %#v", capture.records)
	}
}

// Which terminal states are captured, and — more importantly — which are not.
func TestOnlyTheOutcomesAPayloadExplainsAreCaptured(t *testing.T) {
	if !capturesPayload("provider_error") || !capturesPayload("unsupported_feature") {
		t.Fatal("an outcome the payload explains is not captured")
	}
	for _, outcome := range []string{"success", "rejected", "token_guard_rejected", "accounting_error"} {
		if capturesPayload(outcome) {
			t.Fatalf("%q is captured and should not be", outcome)
		}
	}
	// The one that would defeat the control it sits beside: policy_rejected is
	// redaction refusing an answer, and storing the content a policy just
	// refused would make the capture the leak the policy exists to prevent.
	if capturesPayload("policy_rejected") {
		t.Fatal("the content a redaction policy refused would be stored")
	}
}

// A refusal that never reached an upstream has nothing to reproduce, and is
// produced at a runaway client's own rate — which is how a bounded store fills
// in minutes.
func TestPolicyRefusalsCaptureNothing(t *testing.T) {
	f := newFixture(t, 1)
	defer f.close()
	capture := withCapture(t, &f)

	for range 20 {
		if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
			t.Fatal("the exhausted budget admitted a request")
		}
	}
	if len(capture.records) != 0 {
		t.Fatalf("%d budget refusals were captured", len(capture.records))
	}
}

// An upstream that answered and an answer Halro could not put on the wire. The
// capture keeps the answer, which is the whole diagnosis, rather than an
// upstream error that did not happen.
func TestAnUnrenderableAnswerCapturesTheAnswer(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	capture := withCapture(t, &f)

	_, err := f.service.generate(
		context.Background(), f.plaintext, "chat", chatCanonical(t),
		func(semantic.GenerateResult) error { return errors.New("wire form cannot carry this content kind") },
	)
	if err == nil {
		t.Fatal("a render that failed answered the caller successfully")
	}
	if len(capture.records) != 1 || len(capture.records[0].Response) == 0 {
		t.Fatalf("the safe response shape was not captured: %#v", capture.records)
	}
	if strings.Contains(string(capture.records[0].Response), "hello") {
		t.Fatal("provider-written response prose was captured")
	}
}

// Capture is off unless an operator turned it on, and off means the gateway
// never touches the store — including on the paths that would otherwise hold a
// reference to the caller's request.
func TestCaptureIsOffByDefaultAndHoldsNothing(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	if f.service.failureCapture != nil {
		t.Fatal("a fixture with no capture configured got one")
	}
	f.adapter.err = &provider.Error{Class: provider.ErrorConnect, Message: "refused"}
	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("the failure did not reach the caller")
	}
}

// Diagnostics must never change what the caller is told. A store that cannot be
// written drops the capture and says so once; the request keeps the answer it
// already had.
func TestAStoreThatCannotBeWrittenDoesNotChangeTheAnswer(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	logs := captureLogs(t, &f)
	capture := withCapture(t, &f)
	capture.err = errors.New("no space left on device")
	f.adapter.err = &provider.Error{
		Class: provider.ErrorBadRequest, StatusCode: 400, Message: "provider error (400): refused",
	}

	_, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) || gatewayErr.HTTPStatus != 400 {
		t.Fatalf("a failed capture changed the caller's answer: %v", err)
	}
	if !strings.Contains(logs.String(), "failure capture was not stored") {
		t.Fatalf("the dropped capture was not reported: %s", logs.String())
	}
	// The thing that failed to be written must not be written into the log
	// instead.
	if strings.Contains(logs.String(), "hello") {
		t.Fatalf("a dropped capture's payload reached the log: %s", logs.String())
	}
}

// blockingCapture holds the write open until it is released, which is what a
// stalled data directory does.
type blockingCapture struct {
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
}

func (c *blockingCapture) PutContext(ctx context.Context, _ failurecapture.Record) (bool, error) {
	c.enteredOnce.Do(func() { close(c.entered) })
	select {
	case <-c.release:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (c *blockingCapture) Saturated() bool { return false }

// A capture that cannot complete must cost this goroutine and nothing else.
// Written inside finalize, it held the project's concurrency slot and the Token
// Guard lease across a file write with no deadline, so a stalled data directory
// turned a diagnostic into a data-plane concurrency stall — and delayed the
// caller's answer by the write on top of it.
func TestASlowCaptureDoesNotHoldTheRequestsLeases(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	blocking := &blockingCapture{entered: make(chan struct{}), release: make(chan struct{})}
	f.service.failureCapture = blocking
	f.service.startFailureCapture()
	f.adapter.err = &provider.Error{
		Class: provider.ErrorBadRequest, StatusCode: 400, Message: "provider error (400): refused",
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
			t.Error("the provider failure did not reach the caller")
		}
	}()

	select {
	case <-blocking.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the capture was never attempted")
	}
	// The ledger is closed out before the write begins, so the request is no
	// longer in flight while the store is stuck.
	if active := activeRequestsAfterReplay(t, f.log); active != 0 {
		t.Fatalf("%d requests are still in flight while a capture is stuck", active)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a slow capture delayed the caller")
	}
	close(blocking.release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.service.ShutdownFailureCapture(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestFailureCaptureShutdownCancelsAnActiveWrite(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	blocking := &blockingCapture{entered: make(chan struct{}), release: make(chan struct{})}
	f.service.failureCapture = blocking
	f.service.startFailureCapture()
	f.adapter.err = &provider.Error{Class: provider.ErrorBadRequest, StatusCode: 400, Message: "refused"}

	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("the provider failure did not reach the caller")
	}
	select {
	case <-blocking.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the capture was never attempted")
	}
	// One active write plus a fixed-size queue is the entire resource cost of a
	// failure storm; enqueue never creates a goroutine per failed request.
	for index := 0; index < failureCaptureQueueCapacity+10; index++ {
		f.service.enqueueCapture(failurecapture.Record{RequestID: "queued", ProjectID: "project_1"})
	}
	if queued := len(f.service.captureQueue); queued != failureCaptureQueueCapacity {
		t.Fatalf("bounded capture queue length = %d, want %d", queued, failureCaptureQueueCapacity)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.service.ShutdownFailureCapture(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown error = %v, want cancellation", err)
	}
	select {
	case <-f.service.captureDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled capture worker did not stop")
	}
}
