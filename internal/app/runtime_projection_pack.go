package app

import (
	"math"
	"math/bits"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// projectionPackPollInterval is how often the page packs' fill is read.
	projectionPackPollInterval = time.Second
	// projectionPackMinCheckpointGap spaces fill-triggered checkpoints, so a
	// pack that cannot be reclaimed (no repack reservation, or live rows filling
	// it) does not run one every poll.
	projectionPackMinCheckpointGap = 5 * time.Second
	// projectionPackShedCheckpointGap spaces checkpoints while new work is shed.
	projectionPackShedCheckpointGap = time.Second
	// projectionPackRecordsPerRequest bounds the Ledger records one admitted
	// request may still produce (acceptance, attempts, settlement, finalization).
	projectionPackRecordsPerRequest = 8
)

// projectionPackReader is a page pack whose obsolete pages only a common
// checkpoint's repack reclaims.
type projectionPackReader interface {
	PackUsage() (used, usable uint64, ok bool)
}

// projectionPackFill triggers a common checkpoint once any watched page pack
// has used half of the room it had after the last checkpoint. The scheduled
// interval alone lets a burst fill a pack first; a full Usage pack cannot
// reduce the backlog a checkpoint must publish, and a full Ledger pack refuses
// the Apply inside an append, so reclamation has to start well before that.
type projectionPackFill struct {
	baseline [2]uint64
	last     time.Time
}

func (f *projectionPackFill) due(packs [2]projectionPackReader, now time.Time) bool {
	if now.Sub(f.last) < projectionPackMinCheckpointGap {
		return false
	}
	for i, p := range packs {
		if p == nil {
			continue
		}
		used, usable, ok := p.PackUsage()
		if ok && usable > f.baseline[i] && used >= f.baseline[i]+(usable-f.baseline[i])/2 {
			f.last = now
			return true
		}
	}
	return false
}

// dueWhileShedding runs a checkpoint at the shorter gap while new work waits.
func (f *projectionPackFill) dueWhileShedding(now time.Time) bool {
	if now.Sub(f.last) < projectionPackShedCheckpointGap {
		return false
	}
	f.last = now
	return true
}

// reclaimed records each pack's use right after a successful checkpoint as the
// baseline the next trigger measures from.
func (f *projectionPackFill) reclaimed(packs [2]projectionPackReader) {
	for i, p := range packs {
		if p == nil {
			continue
		}
		if used, _, ok := p.PackUsage(); ok {
			f.baseline[i] = used
		}
	}
}

// projectionPacks returns the selected Usage and Ledger projection packs.
func (r *Runtime) projectionPacks() [2]projectionPackReader {
	var packs [2]projectionPackReader
	if r.usage != nil && r.usage.disk != nil {
		packs[0] = r.usage.disk
	}
	if r.selectedState != nil && r.selectedState.generation != nil && r.selectedState.generation.Owners.Ledger != nil {
		packs[1] = r.selectedState.generation.Owners.Ledger
	}
	return packs
}

// projectionPackAdmission sheds new business while a projection page pack
// lacks room for what admitted work can still write into it. Only a published
// checkpoint can repack a pack, and a full pack blocks that publication (Usage)
// or latches accounting recovery (Ledger), so a pack must never reach full: new
// work waits with 503 and Retry-After, exactly as during a checkpoint, while
// admitted requests finish, the collector catches up and an immediate
// checkpoint reclaims obsolete pages.
type projectionPackAdmission struct {
	once     sync.Once
	wake     chan struct{}
	shedding atomic.Bool
	// bootstrap is held by the one request admitted while a pack has no
	// observed record (board §58).
	bootstrap atomic.Bool
}

func (a *projectionPackAdmission) wakeup() chan struct{} {
	a.once.Do(func() { a.wake = make(chan struct{}, 1) })
	return a.wake
}

func (a *projectionPackAdmission) shed() {
	a.shedding.Store(true)
	select {
	case a.wakeup() <- struct{}{}:
	default:
	}
}

// admit decides one request. Without a shortfall it admits; a pack with room
// but no observed record admits only the holder of the bootstrap slot, which
// release frees when the request has finished.
func (a *projectionPackAdmission) admit(short, unobserved bool) (release func(), ok bool) {
	switch {
	case short:
		return nil, false
	case !unobserved:
		a.shedding.Store(false)
		return func() {}, true
	case a.bootstrap.CompareAndSwap(false, true):
		return func() { a.bootstrap.Store(false) }, true
	}
	return nil, false
}

// projectionPackShortfall reports whether a page pack written for every
// admitted request lacks room for twice the largest observed bytes per record
// times the records it still owes. The Usage projection owes the unreduced
// Ledger suffix plus a bound for every admitted request. The Ledger projection
// applies each record synchronously inside the append, so it owes the
// per-request bound.
//
// A pack with room but no observed record reports unobserved instead: the
// caller then admits one request at a time through a single bootstrap slot.
// Observations live in memory, so every reopen starts without them, and
// admitting unchecked work let a full pack's first burst push Usage past where
// it can catch up, which a checkpoint cannot reclaim before Usage has caught up
// (board §58). The request holding the slot produces the first observation.
func (r *Runtime) projectionPackShortfall() (short, unobserved bool) {
	if r.selectedState == nil || r.ledger == nil {
		return false, false
	}
	l := &r.liveCheckpoint
	l.mu.Lock()
	active := l.active
	l.mu.Unlock()
	requests := saturatingMul(active, projectionPackRecordsPerRequest)
	if r.usage != nil && r.usage.disk != nil {
		used, usable, perRecord, applied := r.usage.disk.PackAdmission()
		head, _, _ := r.ledger.ChainHead()
		var backlog uint64
		if head.Sequence > applied {
			backlog = head.Sequence - applied
		}
		s, u := packShortfall(used, usable, perRecord, saturatingAdd(backlog, requests))
		short, unobserved = short || s, unobserved || u
	}
	if r.selectedState.generation != nil {
		if owner := r.selectedState.generation.Owners.Ledger; owner != nil {
			used, usable, perRecord := owner.PackAdmission()
			s, u := packShortfall(used, usable, perRecord, requests)
			short, unobserved = short || s, unobserved || u
		}
	}
	return short, unobserved && !short
}

// packShortfall judges one pack. Without an observed record it judges only
// whether the pack has passed its usable room, and reports unobserved.
func packShortfall(used, usable, perRecord, owed uint64) (short, unobserved bool) {
	if usable == 0 {
		return false, false
	}
	if perRecord == 0 {
		return used >= usable, used < usable
	}
	var room uint64
	if used < usable {
		room = usable - used
	}
	return room < saturatingMul(saturatingMul(perRecord, owed), 2), false
}

func saturatingAdd(a, b uint64) uint64 {
	if sum, carry := bits.Add64(a, b, 0); carry == 0 {
		return sum
	}
	return math.MaxUint64
}

func saturatingMul(a, b uint64) uint64 {
	if hi, lo := bits.Mul64(a, b); hi == 0 {
		return lo
	}
	return math.MaxUint64
}

// withProjectionPackAdmission runs inside the checkpoint ticket, so the
// request being decided is already counted among the admitted.
func (r *Runtime) withProjectionPackAdmission(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/health/live" || request.URL.Path == "/health/ready" {
			next.ServeHTTP(w, request)
			return
		}
		if release, ok := r.projectionPack.admit(r.projectionPackShortfall()); ok {
			defer release()
			next.ServeHTTP(w, request)
			return
		}
		r.projectionPack.shed()
		w.Header().Set("Retry-After", "1")
		http.Error(w, "memory state page reclamation is in progress", http.StatusServiceUnavailable)
	})
}
