package metadatastate

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// The business facade preserves the real Metadata class table. C/E rows may
// change locally, but must never appear in a format2 native/replicated frame.
// Unknown and journal-bookkeeping writes remain forbidden.
func validateFacadeOperations(ops []metadatajournal.Op) error {
	if err := metadatajournal.ValidateBoundOperations(ops); err != nil {
		return err
	}
	for _, op := range ops {
		class, err := boltstore.MetadataBaselineClass(op.Path, op.Key)
		if err != nil {
			return err
		}
		switch class {
		case "authoritative", "session", "key-envelope":
			if err := validateDelta([]metadatajournal.Op{op}); err != nil {
				return err
			}
		case "node-derived", "node-local":
			path := make([][]byte, len(op.Path))
			for i, part := range op.Path {
				path[i] = []byte(part)
			}
			if _, err := RowKey(path, nil, true); err != nil {
				return err
			}
			if op.Kind == metadatajournal.OpPut || op.Kind == metadatajournal.OpDelete {
				if _, err := RowKey(path, op.Key, false); err != nil {
					return err
				}
			}
		default:
			return ErrSource
		}
	}
	return nil
}

// The owner lock covers the read pin, entire staged transaction and commit.
// Mixed writes have one atomic content root and one authoritative native frame;
// pure local writes replace that content root at the exact existing native cut.
func (b *Baseline) commitFacadeLocked(ctx context.Context, ops []metadatajournal.Op) (metadatajournal.Record, error) {
	if err := validateFacadeOperations(ops); err != nil {
		return metadatajournal.Record{}, err
	}
	journalOps := make([]metadatajournal.Op, 0, len(ops))
	for _, op := range ops {
		class, err := boltstore.MetadataBaselineClass(op.Path, op.Key)
		if err != nil {
			return metadatajournal.Record{}, err
		}
		if class == "authoritative" || class == "session" || class == "key-envelope" {
			journalOps = append(journalOps, op)
		}
	}
	if len(journalOps) != 0 {
		return b.appendFacadeFrameLocked(ctx, journalOps, ops, nil)
	}
	return metadatajournal.Record{}, b.commitLocalFacadeLocked(ctx, ops)
}

func (b *Baseline) commitLocalFacadeLocked(ctx context.Context, ops []metadatajournal.Op) (resultErr error) {
	head := b.journal.Head()
	if head != b.position {
		return ErrSource
	}
	root, err := b.store.RootIdentity(ctx)
	if err != nil {
		return err
	}
	work, err := b.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: DeltaRAMBytes})
	if err != nil {
		return err
	}
	succeeded := false
	b.ready.Store(false)
	defer func() {
		resultErr = errors.Join(resultErr, work.Release())
		b.ready.Store(succeeded && resultErr == nil)
	}()
	physical, err := b.store.PreparePhysicalUpdate(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, physical.Finish(false, resultErr)) }()
	ctx = physical.Bind(ctx)
	_, resultErr = b.store.UpdateDerived(ctx, root.Cut, func(tx *authenticatedstate.Transaction) error {
		return reduceDelta(tx, metadatajournal.Record{Epoch: head.Epoch, Sequence: head.Sequence, Ops: ops})
	}, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if b.journal.Head() != head {
			return ErrSource
		}
		return b.journal.HealthError()
	})
	succeeded = resultErr == nil
	return resultErr
}
