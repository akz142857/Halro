package provider

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrStreamIdle ends a Provider stream that sent nothing for longer than the
// idle limit after its response headers.
//
// Before this, an upstream that answered 200, sent a few events and then went
// quiet without closing was bounded only by gateway.stream_max_duration, and
// for all of that time the attempt held its reservation, its Project and
// Provider concurrency, a goroutine and a connection.
var ErrStreamIdle = errors.New("provider stream sent nothing within the idle limit")

// StreamIdleFailure is the error an adapter returns when reading a stream
// failed because the idle limit closed it, and nil otherwise.
//
// It is a timeout and it is ambiguous: the request reached the upstream, which
// may be generating and billing still, so it is neither retried nor sent to a
// fallback, even when the caller has not seen a byte. An adapter's own read
// error would say malformed, and on some paths not ambiguous before the first
// event, which is why each one asks this first.
func StreamIdleFailure(err error) *Error {
	if !errors.Is(err, ErrStreamIdle) {
		return nil
	}
	return &Error{Class: ErrorTimeout, Ambiguous: true, Message: "provider stream idle", Cause: ErrStreamIdle}
}

// StreamIdleTransport bounds the gap between bytes of a Provider stream.
//
// It applies to a 2xx answer to a request that asked for a stream, judged by
// the Accept header Halro's own adapters set rather than by what the upstream
// says it sent, so an upstream that mislabels its stream is bounded all the
// same. Every other response passes through untouched: a non-streaming body is
// bounded by the route deadline, and its bytes arrive together.
//
// The clock starts when the headers arrive — before that,
// gateway.attempt_response_header_timeout governs — and restarts on every
// read that returns a byte. It counts bytes, not events, so an SSE comment or
// an `event: ping` keeps a stream alive without Halro knowing any vendor's
// keep-alive format.
type StreamIdleTransport struct {
	Next http.RoundTripper
	Idle time.Duration
}

func (t StreamIdleTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.Next.RoundTrip(request)
	if err != nil || t.Idle <= 0 || response.StatusCode < 200 || response.StatusCode >= 300 ||
		!asksForStream(request.Header.Get("Accept")) {
		return response, err
	}
	response.Body = newIdleBody(response.Body, t.Idle)
	return response, nil
}

// CloseIdleConnections passes through, because http.Client only reaches a
// transport's idle pool when the transport itself has the method: without it
// an adapter's Close would leave the pinned connections open.
func (t StreamIdleTransport) CloseIdleConnections() {
	if closer, ok := t.Next.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// Unwrap is the transport this one bounds, so what inspects a client's
// transport — safetransport.PolicyOf — still sees the policy beneath.
func (t StreamIdleTransport) Unwrap() http.RoundTripper { return t.Next }

// asksForStream reports an Accept header naming a stream: SSE, or the AWS
// event stream Bedrock answers converse-stream with.
func asksForStream(accept string) bool {
	for part := range strings.SplitSeq(accept, ",") {
		mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		if mediaType == "text/event-stream" || mediaType == "application/vnd.amazon.eventstream" {
			return true
		}
	}
	return false
}

type idleBody struct {
	body    io.ReadCloser
	idle    time.Duration
	timer   *time.Timer
	expired atomic.Bool
	once    sync.Once
	err     error
}

func newIdleBody(body io.ReadCloser, idle time.Duration) *idleBody {
	b := &idleBody{body: body, idle: idle}
	b.timer = time.AfterFunc(idle, b.expire)
	return b
}

// expire closes the body under a read that is waiting for bytes that are not
// coming, which is the only way to make that read return.
func (b *idleBody) expire() {
	if b.expired.CompareAndSwap(false, true) {
		_ = b.close()
	}
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if b.expired.Load() {
		// Whatever the closed body said — a closed-body error, a reset, even
		// EOF raced against the timer — the stream was ended here.
		b.timer.Stop()
		return n, ErrStreamIdle
	}
	if err != nil {
		b.timer.Stop()
		return n, err
	}
	if n > 0 {
		b.timer.Reset(b.idle)
	}
	return n, nil
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	return b.close()
}

func (b *idleBody) close() error {
	b.once.Do(func() { b.err = b.body.Close() })
	return b.err
}
