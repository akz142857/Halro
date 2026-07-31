package ledger

import "sync/atomic"

type AccountingStatus uint32

const (
	AccountingHealthy AccountingStatus = iota
	AccountingDegraded
	AccountingUnavailable
	AccountingRecoveryRequired
)

type Status struct {
	value atomic.Uint32
}

func NewStatus() *Status {
	status := &Status{}
	status.value.Store(uint32(AccountingHealthy))
	return status
}

func (s *Status) Load() AccountingStatus {
	return AccountingStatus(s.value.Load())
}

func (s *Status) MarkDegraded() {
	s.advance(AccountingDegraded)
}

func (s *Status) MarkUnavailable() {
	s.advance(AccountingUnavailable)
}

func (s *Status) RequireRecovery() {
	s.advance(AccountingRecoveryRequired)
}

func (s *Status) MarkHealthyAfterVerifiedRecovery() {
	s.value.Store(uint32(AccountingHealthy))
}

func (s *Status) advance(next AccountingStatus) {
	for {
		current := s.value.Load()
		if current >= uint32(next) {
			return
		}
		if s.value.CompareAndSwap(current, uint32(next)) {
			return
		}
	}
}
