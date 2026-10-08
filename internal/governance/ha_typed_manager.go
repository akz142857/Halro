package governance

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// NewHASelectedTypedProjectedManager binds the genuine selected Primary owner.
// Source freezing precedes Member capture so native durability cannot invert
// those locks. The application still owns quorum confirmation and admission.
func NewHASelectedTypedProjectedManager(ctx context.Context, native *Log, projection *GovernanceProjection, accounting ledger.AccountingWorkUnitReader, definitions DefinitionReader, manager *resourcebudget.Manager) (*Manager, error) {
	return newHASelectedTypedManager(ctx, native, projection, accounting, definitions, manager, false)
}

// NewHASelectedTypedReadManager permits a Replica's actual applied prefix even
// when a received tail exists. Report is permanently refused by this owner.
func NewHASelectedTypedReadManager(ctx context.Context, native *Log, projection *GovernanceProjection, accounting ledger.AccountingWorkUnitReader, definitions DefinitionReader, manager *resourcebudget.Manager) (*Manager, error) {
	return newHASelectedTypedManager(ctx, native, projection, accounting, definitions, manager, true)
}

func newHASelectedTypedManager(ctx context.Context, native *Log, projection *GovernanceProjection, accounting ledger.AccountingWorkUnitReader, definitions DefinitionReader, manager *resourcebudget.Manager, readOnly bool) (result *Manager, resultErr error) {
	if native == nil || native.startupProvenance == nil || native.startupProvenance.ha == nil || manager == nil || projection == nil {
		return nil, ErrUnavailable
	}
	h := native.startupProvenance.ha
	role := authenticatedstate.SealPrimary
	if readOnly {
		role = authenticatedstate.SealReplica
	}
	if h.role != role || h.withReadAuthority == nil {
		return nil, ErrUnavailable
	}
	if err := native.validateSelectedGrant(ctx, true); err != nil {
		return nil, err
	}
	resultErr = native.WithFrozenHead(ctx, manager, func(ctx context.Context, full Summary) error {
		return h.withScope(ctx, func(ctx context.Context, a authenticatedstate.SealEligibility, allowed HASelectedNativePosition) error {
			if a.Role != role || a.Instance != h.initialAuthority.Instance || a.Cluster != h.initialAuthority.Cluster || a.Member != h.initialAuthority.Member || a.Term != h.initialAuthority.Term || a.Incarnation != h.initialAuthority.Incarnation || a.OrderingIndex < h.initialAuthority.OrderingIndex || !native.selectedProjectionMode(ctx, a, true) {
				return ErrUnavailable
			}
			permitted, err := native.resolveHASelectedPrefix(ctx, a.Role, NativeStartupReceipt{Saved: native.startupSaved, Head: full}, allowed)
			if err != nil {
				return err
			}
			if err := projection.ValidateTypedSource(native, manager); err != nil {
				return err
			}
			projected, err := projection.ReadHead(ctx)
			if err != nil || projected != permitted {
				return errors.Join(ErrUnavailable, err)
			}
			result, err = newProjectedManager(ctx, native, projection, accounting, definitions, typedProjectionCheckpoint{native: native, projection: projection, budget: manager}, true, readOnly)
			return err
		})
	})
	if resultErr != nil && result != nil {
		resultErr = errors.Join(resultErr, result.Retire(context.Background()))
		result = nil
	}
	return
}
