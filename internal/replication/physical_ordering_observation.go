package replication

import (
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// PhysicalOrderingWindowObservation proves this original bank's current range.
// It is read-only bookkeeping and cannot authorize role, confirmation or IO.
type PhysicalOrderingWindowObservation struct {
	MemberBanks       [2]resourcebudget.PhysicalIdentity
	Ordering, Control resourcebudget.PhysicalIdentity
	End               int64
	Index             uint64
	Head              [32]byte
	RemainingRecords  uint32
}

func (o *PhysicalPublication) ObservePhysicalOrderingWindowFrozen(ctx context.Context) (out PhysicalOrderingWindowObservation, resultErr error) {
	if o == nil {
		return out, ErrMemberAuthority
	}
	resultErr = borrowMemberWholeReadScope(ctx, o.publisher, func(m MemberState) error {
		j := o.journal
		j.mu.Lock()
		defer j.mu.Unlock()
		if err := o.validatePeerControlLocked(ctx, m); err != nil {
			return err
		}
		if o.reserved < 0 || o.end < j.nextOffset {
			return ErrMemberAuthority
		}
		remaining := (o.end-j.nextOffset)/orderingRecordBytes - int64(o.reserved)
		if remaining < 0 || remaining > maxUnconfirmedEntries {
			return ErrMemberAuthority
		}
		if err := resourcebudget.CheckPhysicalRange(j.file, j.nextOffset, o.end); err != nil {
			return err
		}
		out = PhysicalOrderingWindowObservation{MemberBanks: o.receipt.State, Ordering: o.receipt.Ordering, Control: o.receipt.Receipt, End: o.end, Index: m.DurableIndex, Head: m.OrderingHeadMAC, RemainingRecords: uint32(remaining)}
		return errors.Join(o.receiptOwner.VerifyFile(o.receiptFile), ctx.Err())
	})
	if resultErr != nil {
		out = PhysicalOrderingWindowObservation{}
	}
	return
}
