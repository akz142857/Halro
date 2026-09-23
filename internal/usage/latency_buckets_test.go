package usage

import (
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

// TestLatencyBucketsAreAscendingAndDistinct. A histogram counts into the first
// bound a sample fits under, so an out-of-order or repeated bound would silently
// make a bucket unreachable.
func TestLatencyBucketsAreAscendingAndDistinct(t *testing.T) {
	for index := 1; index < len(LatencyBucketsMillis); index++ {
		if LatencyBucketsMillis[index] <= LatencyBucketsMillis[index-1] {
			t.Fatalf("bound %d (%d) does not exceed bound %d (%d)",
				index, LatencyBucketsMillis[index], index-1, LatencyBucketsMillis[index-1])
		}
	}
	if len(LatencyBucketsMillis) != domain.RollupLatencyBuckets {
		t.Fatalf("the ladder has %d bounds and a rollup row holds %d",
			len(LatencyBucketsMillis), domain.RollupLatencyBuckets)
	}
}

// TestTheDefaultDeadlinesAreOnBucketEdges is why the three bounds above 30s
// exist.
//
// The ladder used to step 30000 → 120000, one bucket spanning a minute and a
// half — exactly the range a request occupies when this instance's own
// deadlines cut it, since the shipped attempt deadline is 1m0s and the shipped
// request budget is 2m0s. "Did that request hit my attempt timeout?" is only
// answerable from a histogram if the timeout is a bucket edge; without one,
// 55s and 90s report the same number and an operator has to read a per-attempt
// latency column by eye instead.
func TestTheDefaultDeadlinesAreOnBucketEdges(t *testing.T) {
	edges := make(map[uint64]struct{}, len(LatencyBucketsMillis))
	for _, bound := range LatencyBucketsMillis {
		edges[bound] = struct{}{}
	}
	for _, deadline := range []uint64{60_000, 120_000} {
		if _, found := edges[deadline]; !found {
			t.Fatalf("%dms is a shipped deadline and is not a bucket edge: %v", deadline, LatencyBucketsMillis)
		}
	}
	// And the span either side of the attempt deadline is resolvable rather
	// than being one wide bucket.
	var between int
	for _, bound := range LatencyBucketsMillis {
		if bound > 30_000 && bound <= 120_000 {
			between++
		}
	}
	if between < 4 {
		t.Fatalf("only %d bounds cover 30s to 120s; that range is where a request sits when a deadline cuts it", between)
	}
}

// TestRecordLatencyCountsIntoTheNewBounds. The re-cut is only worth its version
// bumps if a sample between the old bounds actually lands somewhere new.
func TestRecordLatencyCountsIntoTheNewBounds(t *testing.T) {
	var buckets [latencyBucketCount]uint64
	// 55s and 90s used to be indistinguishable: both fell in the single bucket
	// that spanned 30s to 120s.
	recordLatency(&buckets, 55_000)
	recordLatency(&buckets, 90_000)
	counted := map[uint64]uint64{}
	for index, count := range buckets {
		if count > 0 {
			counted[LatencyBucketsMillis[index]] = count
		}
	}
	if len(counted) != 2 {
		t.Fatalf("55s and 90s landed in %d bucket(s): %v", len(counted), counted)
	}
	if counted[60_000] != 1 || counted[90_000] != 1 {
		t.Fatalf("expected one sample under 60s and one under 90s, got %v", counted)
	}
}
