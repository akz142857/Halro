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

// RequireRecovery latches: nothing lowers the status again within the life of
// the process.
//
// The reset is a restart, and deliberately so. A fresh process builds a fresh
// Status and re-opens the WAL, which re-scans it, re-checks every frame's CRC,
// and re-authenticates the chain against the trusted checkpoint — so the status
// comes back healthy only because the log was examined again and found sound.
// An in-process "mark healthy" would clear the flag without re-establishing any
// of that, which is the shape of a fail-open: the one thing standing between a
// corrupt ledger and continued accounting would be a call somebody could make.
func (s *Status) RequireRecovery() {
	s.advance(AccountingRecoveryRequired)
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
