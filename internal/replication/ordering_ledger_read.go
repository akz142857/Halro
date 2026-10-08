package replication

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type orderingLedgerReadOwner struct {
	cancel context.CancelFunc
	done   chan struct{}
	credit resourcebudget.Permit
}

// WithLedgerReadPrefix holds the real Ordering writer mutex for one finite
// single-Ledger permission callback. Concurrent native durability publication
// waits for the read rather than failing ErrOrderingFrozen after fsync. It
// authenticates the complete actual chain and joins Close before releasing its
// descriptor/credit. The callback must not reenter Ordering or Close it.
func (j *OrderingJournal) WithLedgerReadPrefix(ctx context.Context, budget *resourcebudget.Manager, through, boundaryIndex uint64, visit func(context.Context, FrozenOrderingHead, StoreCursor, OrderingRecord) error) (resultErr error) {
	return j.withStoreReadPrefix(ctx, budget, through, boundaryIndex, StoreLedger, visit)
}

func (j *OrderingJournal) WithGovernanceReadPrefix(ctx context.Context, budget *resourcebudget.Manager, through, boundaryIndex uint64, visit func(context.Context, FrozenOrderingHead, StoreCursor, OrderingRecord) error) error {
	return j.withStoreReadPrefix(ctx, budget, through, boundaryIndex, StoreGovernance, visit)
}

func (j *OrderingJournal) WithMetadataReadPrefix(ctx context.Context, budget *resourcebudget.Manager, through, boundaryIndex uint64, visit func(context.Context, FrozenOrderingHead, StoreCursor, OrderingRecord) error) error {
	return j.withStoreReadPrefix(ctx, budget, through, boundaryIndex, StoreMetadata, visit)
}

func (j *OrderingJournal) withStoreReadPrefix(ctx context.Context, budget *resourcebudget.Manager, through, boundaryIndex uint64, store Store, visit func(context.Context, FrozenOrderingHead, StoreCursor, OrderingRecord) error) (resultErr error) {
	if j == nil || budget == nil || visit == nil || through < boundaryIndex || boundaryIndex == 0 {
		return ErrOrderingSource
	}
	credit, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: OrderingFrozenReadRAMBytes})
	if err != nil {
		return err
	}
	opCtx, cancel := context.WithCancel(ctx)
	owner := &orderingLedgerReadOwner{cancel: cancel, done: make(chan struct{}), credit: credit}
	j.ledgerReadMu.Lock()
	if j.ledgerReadClosing || j.ledgerRead != nil {
		j.ledgerReadMu.Unlock()
		cancel()
		return errors.Join(ErrOrderingSource, credit.Release())
	}
	j.ledgerRead = owner
	j.ledgerReadMu.Unlock()
	defer func() {
		cancel()
		if err := credit.Release(); err != nil {
			resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, err)
			j.mu.Lock()
			j.poisoned = errors.Join(j.poisoned, resultErr)
			j.invalidateLocked()
			j.mu.Unlock()
		} else {
			j.ledgerReadMu.Lock()
			j.ledgerRead = nil
			j.ledgerReadMu.Unlock()
		}
		close(owner.done)
	}()
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := opCtx.Err(); err != nil {
		return err
	}
	if j.closed || j.closing || j.file == nil || j.poisoned != nil || j.freezeCancel != nil || through > j.lastIndex {
		return errors.Join(ErrOrderingSource, j.poisoned)
	}
	head := FrozenOrderingHead{Header: j.header, Index: j.lastIndex, Term: j.lastTerm, MAC: j.head, Offset: j.nextOffset, StoreCursors: j.tailCursors}
	verify := func() error {
		err := j.verifyFrozenOrderingLocked(opCtx, head)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			j.poisoned = errors.Join(j.poisoned, ErrOrderingSource, err)
			j.invalidateLocked()
		}
		return err
	}
	if err := verify(); err != nil {
		return err
	}
	cursors := j.header.StoreCursors
	var boundary OrderingRecord
	for index := uint64(1); index <= through; index++ {
		if err := opCtx.Err(); err != nil {
			return err
		}
		record, err := j.recordLocked(index)
		if err != nil {
			return err
		}
		cursors, err = advanceOrderingCursors(cursors, record)
		if err != nil {
			return err
		}
		if index == boundaryIndex {
			boundary = record
		}
	}
	resultErr = visit(opCtx, head, cursors[store-StoreLedger], boundary)
	return errors.Join(resultErr, verify(), opCtx.Err())
}
