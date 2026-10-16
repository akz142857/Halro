package budget

import "errors"

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
		Bytes: m.state.HotStateBytes(), Limit: m.hotStateLimit,
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

// admitGrowth refuses an entry that would grow the state while it is at its
// limit.
func (m *Manager) admitGrowth(entry hotStateEntry) error {
	if m.hotStateLimit <= 0 || m.state.HotStateBytes() < m.hotStateLimit {
		return nil
	}
	m.hotStateRejections[entry].Add(1)
	return ErrHotStateFull
}
