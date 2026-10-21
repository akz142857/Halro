package replication

import "time"

// durationBuckets is fixed across HA members and versions of this metric.
// Samples beyond the last boundary are counted in +Inf only.
var durationBuckets = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

func DurationBucketBound(index int) float64 { return durationBuckets[index] }

// DurationHistogram is a process-local, low-cardinality observation. Owners
// synchronize writes and snapshots with their existing lifecycle mutex.
type DurationHistogram struct {
	Buckets [len(durationBuckets)]uint64
	Count   uint64
	Sum     float64
}

func (h *DurationHistogram) observe(duration time.Duration) {
	seconds := duration.Seconds()
	if seconds < 0 {
		seconds = 0
	}
	for index, bound := range durationBuckets {
		if seconds <= bound {
			h.Buckets[index]++
			break
		}
	}
	h.Count++
	h.Sum += seconds
}

type ConfirmationTelemetry struct {
	DurableToConfirmed DurationHistogram
	LastConfirmedAt    time.Time
}
