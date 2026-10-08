package governance

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// NewTypedProjectedManager uses the actual authenticated projection's durable
// native cut as its checkpoint. The node-derived legacy Metadata anchor must
// not be sent through the format2 authoritative Metadata journal. This checks
// the real source and shared owner; an ordinary bbolt projection is ineligible.
// The caller retains role/admission authority and must join this Manager before
// closing the projection or native log.
func NewTypedProjectedManager(ctx context.Context, native *Log, projection *GovernanceProjection, accounting ledger.AccountingWorkUnitReader, definitions DefinitionReader, manager *resourcebudget.Manager) (*Manager, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := projection.ValidateTypedSource(native, manager); err != nil {
		return nil, err
	}
	return NewProjectedManager(ctx, native, projection, accounting, definitions, typedProjectionCheckpoint{native: native, projection: projection, budget: manager})
}

type typedProjectionCheckpoint struct {
	native     *Log
	projection *GovernanceProjection
	budget     *resourcebudget.Manager
}

func (c typedProjectionCheckpoint) PutGovernanceJournalAnchor(sequence uint64, offset int64, hash, authentication [32]byte) error {
	ctx := context.Background()
	if err := c.projection.ValidateTypedSource(c.native, c.budget); err != nil {
		return err
	}
	expected := Summary{Records: sequence, Bytes: offset, LastHash: hash}
	actual, actualMAC := c.native.AuthenticatedHead()
	if expected != actual || authentication != actualMAC {
		return ErrUnavailable
	}
	// ReadHead authenticates the durable COW commitment and its cut row. This
	// is persisted by Apply before Report asks for its checkpoint receipt.
	projected, err := c.projection.ReadHead(ctx)
	if err != nil || projected != expected {
		return errors.Join(ErrUnavailable, err)
	}
	if err := c.native.VerifyHead(ctx, expected); err != nil {
		return err
	}
	final, finalMAC := c.native.AuthenticatedHead()
	if final != expected || finalMAC != authentication {
		return ErrUnavailable
	}
	return nil
}
