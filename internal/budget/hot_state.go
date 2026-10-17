package budget

import (
	"errors"
	"sync"
)

// ErrHotStateFull refuses new work while the accounting state holds more than
// its limit (docs/todo/memory-bounded-redesign.zh-CN.md §14). Work already
// admitted still reserves, settles and finishes, which is what shrinks it.
var ErrHotStateFull = errors.New("accounting state is at its memory limit")

// HotStateStats is the accounting state's estimated size, its limit (zero when
// none is set), and how many new requests, Work Units and Runs were refused.
type HotStateStats struct {
	Bytes              int64
	Limit              int64
	RequestRejections  uint64
	WorkUnitRejections uint64
	RunRejections      uint64
}

func (m *Manager) HotStateStats() HotStateStats {
	return HotStateStats{
		Bytes: m.state.HotStateBytes(m.openRequests.count()), Limit: m.hotStateLimit,
		RequestRejections:  m.hotStateRejections[hotStateRequest].Load(),
		WorkUnitRejections: m.hotStateRejections[hotStateWorkUnit].Load(),
		RunRejections:      m.hotStateRejections[hotStateRun].Load(),
	}
}

type hotStateEntry int

const (
	hotStateRequest hotStateEntry = iota
	hotStateWorkUnit
	hotStateRun
	hotStateEntries
)

// openRequests are the requests this process admitted and has not finalized.
// A request adds nothing to the Ledger state until it reserves, so the
// estimate prices each of these as a reservation from the moment it is let in.
// It is a set rather than a count: a request can be finalized on more than one
// path, and only the first may release it.
type openRequests struct {
	mu   sync.Mutex
	ids  map[string]struct{}
	peak int
}

func (o *openRequests) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.ids)
}

func (o *openRequests) release(requestID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.ids, requestID)
	// Rebuilt once it empties out, as the Ledger state's maps are.
	if o.peak >= 1024 && len(o.ids)*4 <= o.peak {
		fresh := make(map[string]struct{}, len(o.ids))
		for id := range o.ids {
			fresh[id] = struct{}{}
		}
		o.ids, o.peak = fresh, len(o.ids)
	}
}

// admitRequest registers a request and checks the limit with it counted,
// under one lock, so a burst cannot pass the check before any of it is
// counted. The caller releases the request if it is not accepted after all.
func (m *Manager) admitRequest(requestID string) error {
	m.openRequests.mu.Lock()
	defer m.openRequests.mu.Unlock()
	if m.openRequests.ids == nil {
		m.openRequests.ids = map[string]struct{}{}
	}
	if _, open := m.openRequests.ids[requestID]; open {
		return nil
	}
	if m.hotStateLimit > 0 && m.state.HotStateBytes(len(m.openRequests.ids)+1) > m.hotStateLimit {
		m.hotStateRejections[hotStateRequest].Add(1)
		return ErrHotStateFull
	}
	m.openRequests.ids[requestID] = struct{}{}
	m.openRequests.peak = max(m.openRequests.peak, len(m.openRequests.ids))
	return nil
}

// admitGrowth refuses a Work Unit or Run while the state is at its limit.
func (m *Manager) admitGrowth(entry hotStateEntry) error {
	if m.hotStateLimit <= 0 || m.state.HotStateBytes(m.openRequests.count()) < m.hotStateLimit {
		return nil
	}
	m.hotStateRejections[entry].Add(1)
	return ErrHotStateFull
}
