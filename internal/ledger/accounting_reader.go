package ledger

import (
	"context"
	"errors"
)

// AccountingLeaseReader lets startup/maintenance resolve price pins against a
// frozen accounting view without requiring the full in-memory State. Errors
// must never be treated as proof that a lease is absent. The owner establishes
// view trust and excludes business mutations for the entire recovery operation.
type AccountingLeaseReader interface {
	ReadLease(context.Context, string) (Record, bool, error)
	ReadPosition(context.Context) (Watermark, [32]byte, error)
}

var _ AccountingLeaseReader = (*State)(nil)
var _ AccountingLeaseReader = (*LedgerProjection)(nil)

func (s *State) ReadLease(ctx context.Context, id string) (Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, false, err
	}
	if s == nil {
		return Record{}, false, errors.New("accounting State is absent")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return Record{}, false, err
	}
	r, found := s.leaseRecords[id]
	return r, found, nil
}

// Legacy State does not retain a chain hash. Zero explicitly preserves that
// boundary; this adapter is not an authenticated snapshot or a trust protocol.
func (s *State) ReadPosition(ctx context.Context) (Watermark, [32]byte, error) {
	if err := ctx.Err(); err != nil {
		return Watermark{}, [32]byte{}, err
	}
	if s == nil {
		return Watermark{}, [32]byte{}, errors.New("accounting State is absent")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.watermark, [32]byte{}, ctx.Err()
}
