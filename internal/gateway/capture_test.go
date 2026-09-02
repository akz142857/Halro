package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/failurecapture"
	"github.com/akz142857/Halro/internal/provider"
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

	if _, err := f.service.Chat(context.Background(), f.plaintext, chatRequest()); err == nil {
		t.Fatal("the provider failure did not reach the caller")
	}
	if len(capture.records) != 1 {
		t.Fatalf("got %d captures, want 1", len(capture.records))
	}
	record := capture.records[0]
	if record.Outcome != "provider_error" || record.ProjectID != "project_1" || record.RequestID == "" {
		t.Fatalf("record = %#v", record)
	}
	// The request as it went upstream, so the failure can be replayed.
	if !strings.Contains(string(record.Request), "hello") {
		t.Fatalf("the captured request does not hold what was sent: %s", record.Request)
	}
	// The upstream's own sentence. It is refused everywhere else — a provider
	// body is the one place a rejected credential is most likely to be quoted
	// back — and this store is where it can be held under encryption, a clock
	// and an audit.
	var response map[string]any
	if err := json.Unmarshal(record.Response, &response); err != nil {
		t.Fatalf("captured response is not decodable: %s", record.Response)
	}
	if !strings.Contains(response["body"].(string), "Error while downloading") ||
		response["provider_status"] != float64(400) {
		t.Fatalf("the captured response lost the upstream's answer: %v", response)
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
		t.Fatalf("the answer that could not be rendered was not captured: %#v", capture.records)
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
