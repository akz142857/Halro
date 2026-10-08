package statecut

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatastate"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// OpenMetadataStore lends the existing business DAO over the Metadata owner
// selected by this actual common seal. It selects that envelope again before
// factory entry; a raw live DB or caller-supplied head cannot supply provenance.
// Close of this common owner closes/joins the DAO before retiring its Baseline.
// Further native/DAO writes require a newly assembled common cut for publication.
func (r *ReopenedStandalone) OpenMetadataStore(ctx context.Context, authority authenticatedstate.SealEligibility) (*boltstore.Store, error) {
	if r == nil {
		return nil, authenticatedstate.ErrSealIneligible
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.openMetadataStoreLocked(ctx, authority)
}

func (r *ReopenedStandalone) openMetadataStoreLocked(ctx context.Context, authority authenticatedstate.SealEligibility) (*boltstore.Store, error) {
	if r.closed || r.metadataStore != nil || r.cleanupErr != nil || r.Owners.Metadata == nil || r.Owners.Publisher == nil {
		return nil, authenticatedstate.ErrSealIneligible
	}
	selected, found, err := r.Owners.Publisher.Select(authority)
	if err != nil || !found || selected != r.Seal {
		return nil, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	root := r.Seal.Roots[1]
	ref := metadatastate.AuthenticatedMetadataSealReference{Schema: HardLimits().RequiredRoots[1], Root: root, Position: r.Seal.Positions[1]}
	var backend boltstore.FacadeBackend
	if r.selectedStartup {
		backend, err = r.Owners.Metadata.SelectedStartupFacade(ctx, r.Owners.Budget, authority.Instance, r.Owners.InstanceID, r.Owners.KeyGeneration)
	} else {
		backend, err = r.Owners.Metadata.StandaloneFacade(ctx, r.Owners.Budget, authority.Instance, r.Owners.InstanceID, r.Owners.KeyGeneration, ref)
	}
	if err != nil {
		return nil, err
	}
	store, err := boltstore.NewFacadeStore(ctx, backend)
	if err != nil {
		return nil, err
	}
	r.metadataStore = store
	return store, nil
}
