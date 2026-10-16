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
	// The window of recent event digests. It is priced full from the start:
	// it fills within its first 65,536 events and stays full, and pricing it
	// by how full it is would let each event grow the estimate after the
	// admission that let it in was checked.
	hotDigestWindowBytes = 12 << 20
)

// HotStateBytes estimates the heap the state holds. openRequests is how many
// requests the caller has admitted and not yet finalized: a request adds
// nothing to the state until it reserves, so each one is priced as a
// reservation from admission, and a burst admitted before any of it reserves
// is counted as it arrives.
func (s *State) HotStateBytes(openRequests int) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	leases := max(len(s.leaseRecords)-len(s.reservations), 0)
	inFlight := max(len(s.reservations), openRequests)
	return hotDigestWindowBytes +
		int64(inFlight)*hotReservationBytes +
		int64(leases)*hotLeaseBytes +
		int64(len(s.requestRuns))*hotAttributionBytes +
		int64(len(s.workUnits))*hotWorkUnitBytes +
		int64(len(s.runs))*hotRunBytes +
		int64(len(s.governanceIdempotency))*hotIdempotencyBytes +
		int64(len(s.balances))*hotBalanceBytes +
		int64(len(s.indexes.queue))*hotQueuedBytes
}

// Go maps keep their slots after entries are deleted, so a map that a burst
// filled and settlement emptied would hold the burst's slots for good while
// the estimate, counting entries, said it was empty. A map that falls to a
// quarter of its peak is rebuilt at its present size, which frees them: the
// copy costs its remaining entries, paid once per three quarters deleted. The
// copy is made entry by entry into a map sized for what is left; maps.Clone
// keeps the source's table, slots and all.
//
// Measured on a burst of 120,000 reservations that then settled: without the
// rebuild the reservation and lease maps kept 6.4 MiB each with no entries.
const shrinkFloor = 1024

type mapPeaks struct {
	reservations, leaseRecords, requestRuns, workUnits, runs, runsByWorkUnit,
	activeByWorkUnit, pendingByWorkUnit, workUnitCosts, governanceIdempotency,
	idemByUnit, liveRuns, openByProject int
}

func shrink[K comparable, V any](m map[K]V, peak *int) map[K]V {
	if len(m) > *peak {
		*peak = len(m)
	}
	if *peak < shrinkFloor || len(m)*4 > *peak {
		return m
	}
	*peak = len(m)
	return rebuilt(m)
}

// rebuilt copies m into a map sized for its entries.
func rebuilt[K comparable, V any](m map[K]V) map[K]V {
	fresh := make(map[K]V, len(m))
	for key, value := range m {
		fresh[key] = value
	}
	return fresh
}

// shrinkMaps rebuilds the maps that emptied out. The caller holds the lock.
func (s *State) shrinkMaps() {
	p := &s.peaks
	s.reservations = shrink(s.reservations, &p.reservations)
	s.leaseRecords = shrink(s.leaseRecords, &p.leaseRecords)
	s.requestRuns = shrink(s.requestRuns, &p.requestRuns)
	s.workUnits = shrink(s.workUnits, &p.workUnits)
	s.runs = shrink(s.runs, &p.runs)
	s.runsByWorkUnit = shrink(s.runsByWorkUnit, &p.runsByWorkUnit)
	s.activeByWorkUnit = shrink(s.activeByWorkUnit, &p.activeByWorkUnit)
	s.pendingByWorkUnit = shrink(s.pendingByWorkUnit, &p.pendingByWorkUnit)
	s.workUnitCosts = shrink(s.workUnitCosts, &p.workUnitCosts)
	s.governanceIdempotency = shrink(s.governanceIdempotency, &p.governanceIdempotency)
	s.indexes.idemByUnit = shrink(s.indexes.idemByUnit, &p.idemByUnit)
	s.indexes.liveRuns = shrink(s.indexes.liveRuns, &p.liveRuns)
	s.indexes.openByProject = shrink(s.indexes.openByProject, &p.openByProject)
}
