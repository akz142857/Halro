package provider

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countingBody is a pipe whose closes are counted, so a test can see that the
// body under the idle limit is closed once, whoever closes it first.
type countingBody struct {
	*io.PipeReader
	closes atomic.Int32
}

func (b *countingBody) Close() error {
	b.closes.Add(1)
	return b.PipeReader.Close()
}

func newCountingPipe() (*countingBody, *io.PipeWriter) {
	reader, writer := io.Pipe()
	return &countingBody{PipeReader: reader}, writer
}

func TestIdleBodyEndsAStreamThatStopsSending(t *testing.T) {
	body, writer := newCountingPipe()
	defer writer.Close()
	idle := newIdleBody(body, 50*time.Millisecond)
	go func() { _, _ = writer.Write([]byte("data: first\n\n")) }()
	buffer := make([]byte, 64)
	if n, err := idle.Read(buffer); err != nil || string(buffer[:n]) != "data: first\n\n" {
		t.Fatalf("first read = %q, %v", buffer[:n], err)
	}
	started := time.Now()
	_, err := idle.Read(buffer)
	if !errors.Is(err, ErrStreamIdle) {
		t.Fatalf("stalled read error = %v, want ErrStreamIdle", err)
	}
	if waited := time.Since(started); waited > 5*time.Second {
		t.Fatalf("stalled read returned after %s", waited)
	}
	if err := idle.Close(); err != nil {
		t.Fatal(err)
	}
	if closes := body.closes.Load(); closes != 1 {
		t.Fatalf("body closed %d times, want once", closes)
	}
}

// The limit is on the gap, not the total: a stream that keeps sending runs
// for as long as it keeps sending.
func TestIdleBodyLetsAStreamThatKeepsSendingRunPastTheLimit(t *testing.T) {
	body, writer := newCountingPipe()
	const limit = 300 * time.Millisecond
	idle := newIdleBody(body, limit)
	go func() {
		deadline := time.Now().Add(4 * limit)
		for time.Now().Before(deadline) {
			if _, err := writer.Write([]byte(": ping\n")); err != nil {
				return
			}
			time.Sleep(limit / 15)
		}
		_ = writer.Close()
	}()
	started := time.Now()
	if _, err := io.Copy(io.Discard, idle); err != nil {
		t.Fatalf("stream that kept sending ended with %v", err)
	}
	if ran := time.Since(started); ran < 3*limit {
		t.Fatalf("stream ran %s, want past the %s limit", ran, limit)
	}
	if err := idle.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestIdleBodyClosedBeforeTheLimitStaysClosedOnce(t *testing.T) {
	body, writer := newCountingPipe()
	defer writer.Close()
	idle := newIdleBody(body, 20*time.Millisecond)
	if err := idle.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if idle.expired.Load() {
		t.Fatal("the limit fired on a body already closed")
	}
	if closes := body.closes.Load(); closes != 1 {
		t.Fatalf("body closed %d times, want once", closes)
	}
}

type staticRoundTripper struct{ response *http.Response }

func (s staticRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return s.response, nil
}

func TestStreamIdleTransportBoundsOnlyASuccessfulStream(t *testing.T) {
	for _, tc := range []struct {
		name    string
		accept  string
		status  int
		idle    time.Duration
		bounded bool
	}{
		{"SSE", "text/event-stream", 200, time.Minute, true},
		{"SSE with parameters in a list", "application/json, text/event-stream; charset=utf-8", 200, time.Minute, true},
		{"AWS event stream", "application/vnd.amazon.eventstream", 200, time.Minute, true},
		{"not a stream", "application/json", 200, time.Minute, false},
		{"no Accept", "", 200, time.Minute, false},
		{"a refusal", "text/event-stream", 429, time.Minute, false},
		{"no limit", "text/event-stream", 200, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := io.NopCloser(strings.NewReader("x"))
			transport := StreamIdleTransport{
				Next: staticRoundTripper{&http.Response{StatusCode: tc.status, Body: body}},
				Idle: tc.idle,
			}
			request, err := http.NewRequest(http.MethodPost, "https://provider.invalid/v1/chat/completions", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.accept != "" {
				request.Header.Set("Accept", tc.accept)
			}
			response, err := transport.RoundTrip(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if _, bounded := response.Body.(*idleBody); bounded != tc.bounded {
				t.Fatalf("bounded = %t, want %t", bounded, tc.bounded)
			}
		})
	}
}

func TestStreamIdleFailureIsAnAmbiguousTimeout(t *testing.T) {
	if StreamIdleFailure(errors.New("connection reset")) != nil {
		t.Fatal("an ordinary read error was taken for the idle limit")
	}
	failure := StreamIdleFailure(&Error{Class: ErrorMalformed, Message: "decode stream", Cause: ErrStreamIdle})
	if failure == nil || failure.Class != ErrorTimeout || !failure.Ambiguous || failure.Retryable {
		t.Fatalf("idle failure = %#v, want an ambiguous, non-retryable timeout", failure)
	}
	if !errors.Is(failure, ErrStreamIdle) {
		t.Fatal("idle failure does not unwrap to ErrStreamIdle")
	}
}

type idleClosingRoundTripper struct {
	staticRoundTripper
	closed atomic.Int32
}

func (t *idleClosingRoundTripper) CloseIdleConnections() { t.closed.Add(1) }

// http.Client.CloseIdleConnections reaches the pool only through a transport
// that has the method, so the wrapper must pass it on or an adapter's Close
// leaves its connections open.
func TestStreamIdleTransportPassesOnIdleConnectionCleanup(t *testing.T) {
	next := &idleClosingRoundTripper{}
	client := &http.Client{Transport: StreamIdleTransport{Next: next, Idle: time.Minute}}
	client.CloseIdleConnections()
	if next.closed.Load() != 1 {
		t.Fatal("closing the client's idle connections did not reach the transport beneath the idle limit")
	}
}
