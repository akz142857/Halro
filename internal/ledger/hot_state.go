package ledger

// The hot state's size (docs/todo/memory-bounded-redesign.zh-CN.md §14).
//
// It is estimated from entry counts, not read from the heap: reading the heap
// stops the process, and the heap holds far more than this state. Each price
// is what one entry of that kind holds in the heap, measured by
// TestHotStateEstimateTracksTheHeap and rounded up; that test fails when a
// price drifts from what the code now holds.
const (
	// A reservation together with its lease record, priced on a reservation
	// with every field the gateway sets.
	hotReservationBytes = 3072
	// A lease kept after its reservation settled (pin_commit_failed).
	hotLeaseBytes = 2048
	// An in-flight request attributed to a Run.
	hotAttributionBytes = 256
	// A Work Unit with its cost totals and its Run list.
	hotWorkUnitBytes = 448
	// A Run.
	hotRunBytes = 576
	// A governance idempotency record.
	hotIdempotencyBytes = 384
	// A balance: one project on one day.
	hotBalanceBytes = 224
	// An archived Work Unit waiting for the snapshot writer, with its Run.
	hotQueuedBytes = 2048
	// The window of recent event digests, once full.
	hotDigestWindowBytes = 12 << 20
)

// HotStateBytes estimates the heap the state holds.
func (s *State) HotStateBytes() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	leases := max(len(s.leaseRecords)-len(s.reservations), 0)
	digests := int64(hotDigestWindowBytes) * int64(len(s.eventOrder)) / recentEventDigests
	return digests +
		int64(len(s.reservations))*hotReservationBytes +
		int64(leases)*hotLeaseBytes +
		int64(len(s.requestRuns))*hotAttributionBytes +
		int64(len(s.workUnits))*hotWorkUnitBytes +
		int64(len(s.runs))*hotRunBytes +
		int64(len(s.governanceIdempotency))*hotIdempotencyBytes +
		int64(len(s.balances))*hotBalanceBytes +
		int64(len(s.indexes.queue))*hotQueuedBytes
}
