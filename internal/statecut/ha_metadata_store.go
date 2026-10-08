package statecut

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// OpenHAMetadataStore exposes the existing DAO over this actual selected source
// only after its read child has joined the original source owner. Reads qualify
// actual Member/Ordering and the bound Metadata source and compare the actual transaction
// position while the Baseline mutex is held. Replica writes remain excluded.
// Actual Primary writes retain native admission, ordering and confirmation;
// this factory grants no accounting readiness or promotion.
func (r *ReopenedHA) OpenHAMetadataStore(ctx context.Context) (*boltstore.Store, error) {
	if r == nil || (r.Owners.PromotionRecovery != nil || r.Owners.PeerPromotionRecovery != nil || r.Owners.PeerPromotionCompletion != nil) {
		return nil, authenticatedstate.ErrSealIneligible
	}
	return r.openHAMetadataStore(ctx, false)
}

// OpenHAPromotionOperatorStore exposes only the stopped transition's old
// selected read-only DAO for mandatory administrator reauthentication.
func (r *ReopenedHA) OpenHAPromotionOperatorStore(ctx context.Context) (*boltstore.Store, error) {
	if r == nil || r.Owners.PromotionRecovery == nil {
		return nil, authenticatedstate.ErrSealIneligible
	}
	return r.openHAMetadataStore(ctx, true)
}
func (r *ReopenedHA) openHAMetadataStore(ctx context.Context, promotionOperator bool) (result *boltstore.Store, resultErr error) {
	if r == nil {
		return nil, authenticatedstate.ErrSealIneligible
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.advanceErr != nil || !r.metadataBorrowed || r.metadataStore != nil || r.cleanupErr != nil {
		return nil, authenticatedstate.ErrSealIneligible
	}
	owners := r.Owners
	var backend boltstore.FacadeBackend
	resultErr = WithHASelectedCut(ctx, owners, func(ctx context.Context, cut HASelectedCut) error {
		if cut.Seal != r.Seal {
			return authenticatedstate.ErrSealIneligible
		}
		// The real source has joined the selected read child and may have replayed
		// its independently permitted native suffix before issuing a DAO. Qualify
		// that actual root against the current cut; the saved root supplies binding
		// facts only and cannot grant the suffix through candidate fields.
		ref, err := owners.Components.Metadata.SealReference(ctx)
		saved := cut.Seal.Positions[1]
		if err != nil || ref.Schema != HardLimits().RequiredRoots[1] || ref.Position.Component != saved.Component || ref.Position.Epoch != cut.Allowed[3].Generation || ref.Position.Sequence != cut.Allowed[3].Sequence || ref.Position.Hash != cut.Heads[3] || ref.Position.Revision != saved.Revision || ref.Position.MetadataBaseline != saved.MetadataBaseline {
			return errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		role := cut.Authority.Role
		if promotionOperator {
			role = authenticatedstate.SealReplica
		}
		permission := func(ctx context.Context, visit func(context.Context, authenticatedstate.SealPosition) error) error {
			ctx, cancel := context.WithCancel(ctx)
			stop := context.AfterFunc(r.ctx, cancel)
			defer func() { stop(); cancel() }()
			if promotionOperator {
				return WithHASelectedCut(ctx, owners, func(ctx context.Context, actual HASelectedCut) error {
					if actual.Seal != r.Seal || actual.Heads[3] != ref.Position.Hash || actual.Allowed[3].Sequence != ref.Position.Sequence || actual.Allowed[3].Generation != ref.Position.Epoch {
						return authenticatedstate.ErrSealIneligible
					}
					return visit(ctx, ref.Position)
				})
			}
			return withHAMetadataReadAuthority(ctx, owners, ref.Position, role, visit)
		}
		backend, err = owners.Components.Metadata.HAFacade(ctx, owners.Components.Budget, cut.Authority.Instance, owners.Components.InstanceID, owners.Components.KeyGeneration, ref, role, permission)
		if err != nil {
			return err
		}
		return ctx.Err()
	})
	if resultErr == nil {
		result, resultErr = boltstore.NewFacadeStore(ctx, backend)
		if resultErr == nil {
			r.metadataStore = result
		}
	}
	if resultErr != nil && result != nil {
		resultErr = errors.Join(resultErr, result.Close())
		result = nil
	}
	return
}
