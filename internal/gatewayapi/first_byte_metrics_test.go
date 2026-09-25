package gatewayapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/gateway"
	"github.com/akz142857/Halro/internal/usage"
)

func newFirstByteHandler() *Handler {
	return &Handler{firstByte: newStreamFirstByteHistogram()}
}

func sampleFor(t *testing.T, handler *Handler, operation string) StreamFirstByteSample {
	t.Helper()
	for _, sample := range handler.StreamFirstByte() {
		if sample.Operation == operation {
			return sample
		}
	}
	t.Fatalf("no first-byte sample for %q", operation)
	return StreamFirstByteSample{}
}

// TestFirstByteIsRecordedOncePerStream. A stream delivers many events and only
// the first answers the question; latching is what keeps the series a
// first-byte measurement rather than a per-event one.
func TestFirstByteIsRecordedOncePerStream(t *testing.T) {
	handler := newFirstByteHandler()
	clock := handler.startFirstByteClock(context.Background(), firstByteChatCompletions)
	for range 5 {
		clock.delivered()
	}
	if sample := sampleFor(t, handler, "chat_completions"); sample.Count != 1 {
		t.Fatalf("a five-event stream recorded %d samples", sample.Count)
	}
}

// TestAStreamThatNeverDeliveredRecordsNothing.
//
// A wait that never ended is not a first-byte measurement. Recording it would
// let fast failures improve the percentile the SLO is signed against, which is
// the classic way a latency instrument games itself.
func TestAStreamThatNeverDeliveredRecordsNothing(t *testing.T) {
	handler := newFirstByteHandler()
	handler.startFirstByteClock(context.Background(), firstByteChatCompletions)
	if samples := handler.StreamFirstByte(); len(samples) != 0 {
		t.Fatalf("an undelivered stream produced %d samples", len(samples))
	}
}

// TestTheClockStartsAtArrivalNotAtTheHandler.
//
// The streaming handlers are entered after the source limiter, the key guard,
// the bounded body read and the JSON decode. A first-byte SLO is signed against
// what the caller waits, so that time belongs inside the measurement.
func TestTheClockStartsAtArrivalNotAtTheHandler(t *testing.T) {
	handler := newFirstByteHandler()
	arrived := time.Now().Add(-2 * time.Second)
	ctx := context.WithValue(context.Background(), arrivalContextKey{}, arrived)
	handler.startFirstByteClock(ctx, firstByteResponses).delivered()

	sample := sampleFor(t, handler, "responses")
	if sample.SumMillis < 1900 {
		t.Fatalf("the clock measured %dms from an arrival 2s ago", sample.SumMillis)
	}
	// And it landed in a bucket at or above two seconds rather than the first.
	var below uint64
	for index, bound := range usage.LatencyBucketsMillis {
		if bound < 2000 {
			below += sample.Buckets[index]
		}
	}
	if below != 0 {
		t.Fatalf("a two-second wait counted into a sub-two-second bucket: %v", sample.Buckets)
	}
}

// TestAHandlerReachedWithoutTheMiddlewareStillReports. Dropping the sample
// would make a missing mount look like an endpoint nobody streams from.
func TestAHandlerReachedWithoutTheMiddlewareStillReports(t *testing.T) {
	handler := newFirstByteHandler()
	handler.startFirstByteClock(context.Background(), firstByteMessages).delivered()
	if sample := sampleFor(t, handler, "messages"); sample.Count != 1 {
		t.Fatalf("an unstamped request recorded %d samples", sample.Count)
	}
}

// TestWithArrivalStampsTheRequest.
func TestWithArrivalStampsTheRequest(t *testing.T) {
	handler := newFirstByteHandler()
	var stamped bool
	wrapped := handler.WithArrival(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		_, stamped = request.Context().Value(arrivalContextKey{}).(time.Time)
	}))
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if !stamped {
		t.Fatal("the middleware did not stamp the arrival instant")
	}
}

// TestTheFourFacesAreCountedApart. The Anthropic face renders from the semantic
// model where the native one passes events through, so they do different work
// before the first byte and an average across them answers nothing.
func TestTheFourFacesAreCountedApart(t *testing.T) {
	handler := newFirstByteHandler()
	for _, operation := range []firstByteOperation{
		firstByteChatCompletions, firstByteResponses, firstByteMessages, firstByteMessagesNative,
	} {
		handler.startFirstByteClock(context.Background(), operation).delivered()
	}
	if samples := handler.StreamFirstByte(); len(samples) != 4 {
		t.Fatalf("four faces produced %d series", len(samples))
	}
}

// TestSamplesLandOnTheSharedLadder is what makes §5 signable: a histogram
// reports the upper bound of the bucket a percentile falls in, so a signed p95
// that is not a bucket edge cannot be evaluated against Halro's own metrics at
// all. One ladder across every latency series means one set of legal values.
func TestSamplesLandOnTheSharedLadder(t *testing.T) {
	handler := newFirstByteHandler()
	clock := &firstByteClock{
		histogram: handler.firstByte, operation: firstByteChatCompletions,
		arrived: time.Now(),
	}
	handler.firstByte.observe(clock.operation, 40*time.Millisecond)
	sample := sampleFor(t, handler, "chat_completions")
	// 40ms falls under the 50ms bound, which is the ladder's fourth.
	var index int
	for position, bound := range usage.LatencyBucketsMillis {
		if bound == 50 {
			index = position
		}
	}
	if sample.Buckets[index] != 1 {
		t.Fatalf("40ms did not land under the 50ms bound: %v", sample.Buckets)
	}
}

// TestWaitsAboveTheLastBoundOverflowRatherThanBeingLost. Above the ladder the
// only truthful statement is "greater than that bound", and silently counting
// such a sample into the last bucket would claim a latency it never had.
func TestWaitsAboveTheLastBoundOverflowRatherThanBeingLost(t *testing.T) {
	handler := newFirstByteHandler()
	handler.firstByte.observe(firstByteResponses, 10*time.Minute)
	sample := sampleFor(t, handler, "responses")
	if sample.Overflow != 1 || sample.Count != 1 {
		t.Fatalf("a ten-minute wait produced overflow=%d count=%d", sample.Overflow, sample.Count)
	}
	for index, count := range sample.Buckets {
		if count != 0 {
			t.Fatalf("it also counted into bucket %d", index)
		}
	}
}

// TestAStreamingRequestRecordsItsFirstByte walks the real handler rather than
// the recorder, so the measurement is pinned to the place bytes actually reach
// the caller — the flush — and not to a call somebody remembered to add.
func TestAStreamingRequestRecordsItsFirstByte(t *testing.T) {
	handler, err := New(&fakeService{}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hello"}]}`),
	)
	request.Header.Set("Authorization", "Bearer gw_test")
	response := httptest.NewRecorder()
	handler.WithArrival(http.HandlerFunc(handler.ChatCompletions)).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	sample := sampleFor(t, handler, "chat_completions")
	if sample.Count != 1 {
		t.Fatalf("one streaming request recorded %d first-byte samples", sample.Count)
	}
}

// TestAStreamRefusedBeforeAnyByteRecordsNothing. A caller refused before the
// stream starts gets an ordinary JSON error, never an SSE event — so there was
// no first byte, and the series must not claim one.
func TestAStreamRefusedBeforeAnyByteRecordsNothing(t *testing.T) {
	handler, err := New(&fakeService{err: &gateway.Error{
		Code: "budget_exceeded", Message: "daily budget exceeded", HTTPStatus: http.StatusForbidden,
	}}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"chat","stream":true,"messages":[{"role":"user","content":"hello"}]}`),
	)
	request.Header.Set("Authorization", "Bearer gw_test")
	response := httptest.NewRecorder()
	handler.WithArrival(http.HandlerFunc(handler.ChatCompletions)).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	if samples := handler.StreamFirstByte(); len(samples) != 0 {
		t.Fatalf("a refused stream recorded %d samples", len(samples))
	}
}
