package gatewayapi

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/usage"
)

// Measuring how long a streaming caller waits for the first byte it can act on.
//
// This existed nowhere. The production validation plan asks for a signed
// streaming first-byte p95/p99 (§5) and the 2026-09-18 run found the repository
// had no such series at all — finding 260918-PV-F-06, which blocks signing §5,
// which in turn blocks G5. A load harness could have measured it client-side,
// but then the number an operator watches in production and the number that was
// signed would come from two different instruments.
//
// What it measures is deliberately the *caller's* wait, not the upstream's: the
// clock starts when the HTTP handler receives the request and stops after the
// first SSE event has been written and flushed. So it includes authentication,
// routing, budget admission, redaction setup and the upstream's own time to
// first token — everything between "the caller asked" and "the caller saw
// something". That is what an SLO about first byte means.
//
// It does not, on its own, say whose time it was. Separating Halro's share from
// the upstream's needs a second series taken at the provider dispatch, which is
// a different finding and deliberately not built here.
//
// One instrument, not two: the observation happens in the HTTP layer where the
// bytes actually reach the socket, which is the only place that is true for
// every streaming face at once.

// firstByteOperation names a northbound streaming endpoint. It is a closed set
// of four, so the label cannot grow with traffic.
type firstByteOperation string

const (
	firstByteChatCompletions firstByteOperation = "chat_completions"
	firstByteResponses       firstByteOperation = "responses"
	// The Anthropic face has two: one that renders from the semantic model and
	// one that passes the upstream's own events through. They are different
	// code paths with different work before the first byte, so collapsing them
	// would average away the difference an operator is looking for.
	firstByteMessages       firstByteOperation = "messages"
	firstByteMessagesNative firstByteOperation = "messages_native"
)

// streamFirstByteHistogram counts first-byte waits into the same ladder the
// usage histograms use.
//
// The shared ladder is the point rather than a convenience. §5 has to be signed
// in values that Halro's own metrics can be evaluated against, and a histogram
// reports the upper bound of the bucket a percentile falls in — so a signed
// p95 that is not a bucket edge cannot be judged at all. One ladder means one
// set of legal values across every latency series here.
type streamFirstByteHistogram struct {
	mu sync.Mutex
	// buckets is per operation, counted into the ladder's bounds; overflow
	// holds samples above the last bound, where the only truthful statement is
	// "greater than that bound".
	buckets  map[firstByteOperation]*[len(usage.LatencyBucketsMillis)]uint64
	overflow map[firstByteOperation]uint64
	counts   map[firstByteOperation]uint64
	sums     map[firstByteOperation]uint64
}

func newStreamFirstByteHistogram() *streamFirstByteHistogram {
	return &streamFirstByteHistogram{
		buckets:  make(map[firstByteOperation]*[len(usage.LatencyBucketsMillis)]uint64, 4),
		overflow: make(map[firstByteOperation]uint64, 4),
		counts:   make(map[firstByteOperation]uint64, 4),
		sums:     make(map[firstByteOperation]uint64, 4),
	}
}

func (h *streamFirstByteHistogram) observe(operation firstByteOperation, waited time.Duration) {
	if h == nil || waited < 0 {
		return
	}
	millis := uint64(waited.Milliseconds())
	h.mu.Lock()
	defer h.mu.Unlock()
	counts, tracked := h.buckets[operation]
	if !tracked {
		counts = new([len(usage.LatencyBucketsMillis)]uint64)
		h.buckets[operation] = counts
	}
	h.counts[operation]++
	h.sums[operation] += millis
	for index, upperBound := range usage.LatencyBucketsMillis {
		if millis <= upperBound {
			counts[index]++
			return
		}
	}
	h.overflow[operation]++
}

// StreamFirstByteSample is one operation's distribution, for the metrics
// endpoint to render.
type StreamFirstByteSample struct {
	Operation string
	Buckets   [len(usage.LatencyBucketsMillis)]uint64
	Overflow  uint64
	Count     uint64
	SumMillis uint64
}

// StreamFirstByte reports how long streaming callers waited for their first
// byte, per northbound endpoint.
//
// Process-local and lost on restart, like the refusal classification counter
// and unlike anything in the Ledger: this is a performance observation, not an
// accounting fact, and nothing is owed to anyone if it is forgotten.
func (h *Handler) StreamFirstByte() []StreamFirstByteSample {
	if h == nil || h.firstByte == nil {
		return nil
	}
	h.firstByte.mu.Lock()
	defer h.firstByte.mu.Unlock()
	samples := make([]StreamFirstByteSample, 0, len(h.firstByte.buckets))
	for operation, counts := range h.firstByte.buckets {
		samples = append(samples, StreamFirstByteSample{
			Operation: string(operation), Buckets: *counts,
			Overflow:  h.firstByte.overflow[operation],
			Count:     h.firstByte.counts[operation],
			SumMillis: h.firstByte.sums[operation],
		})
	}
	return samples
}

// firstByteClock marks when a streaming request arrived and reports the wait
// once, the first time bytes reach the caller.
//
// "Once" is the whole contract. A stream delivers many events and only the
// first one answers the question, so the recorder latches; a stream that fails
// before any byte reaches the caller reports nothing at all, because a wait
// that never ended is not a first-byte measurement and averaging it in would
// make failures look like latency.
type firstByteClock struct {
	histogram *streamFirstByteHistogram
	operation firstByteOperation
	arrived   time.Time
	reported  bool
}

// delivered is called after the first SSE event has been written *and*
// flushed. Before the flush the bytes are in a buffer, which is not the same as
// the caller having seen them.
func (c *firstByteClock) delivered() {
	if c == nil || c.reported {
		return
	}
	c.reported = true
	c.histogram.observe(c.operation, time.Since(c.arrived))
}

// arrivalKey carries the instant a request reached Halro.
type arrivalContextKey struct{}

// WithArrival stamps the moment a request arrived, before anything is read from
// it.
//
// The streaming handlers could have started their own clock, but they are
// entered after the source limiter, the key guard, the bounded body read and
// the JSON decode — so a caller sending a large body over a slow link would
// have that time excluded from its own first-byte measurement. A first-byte SLO
// is signed against what the caller waits, and this is the only place inside
// Halro where that clock legitimately starts.
func (h *Handler) WithArrival(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		next.ServeHTTP(writer, request.WithContext(
			context.WithValue(request.Context(), arrivalContextKey{}, time.Now())))
	})
}

// startFirstByteClock reads the stamped arrival instant, falling back to now.
//
// The fallback is for a handler reached without the middleware — a test calling
// it directly, or a future mount that forgets it. It under-reports rather than
// reporting nothing, and the alternative, dropping the sample, would make a
// misconfiguration look like an endpoint nobody streams from.
func (h *Handler) startFirstByteClock(ctx context.Context, operation firstByteOperation) *firstByteClock {
	arrived, stamped := ctx.Value(arrivalContextKey{}).(time.Time)
	if !stamped {
		arrived = time.Now()
	}
	return &firstByteClock{histogram: h.firstByte, operation: operation, arrived: arrived}
}
