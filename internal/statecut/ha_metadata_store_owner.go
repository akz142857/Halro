package statecut

import (
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// ValidateHAMetadataStoreOwner compares only the DAO issued by this actual
// selected generation. It grants no content permission; consumers must still
// enter the original source scopes and execute the facade's authenticated read.
func (r *ReopenedHA) ValidateHAMetadataStoreOwner(store *boltstore.Store, manager *resourcebudget.Manager) error {
	if r == nil || store == nil || manager == nil {
		return authenticatedstate.ErrSealIneligible
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx == nil || r.closed || r.ctx.Err() != nil || r.advanceErr != nil || r.cleanupErr != nil || !r.metadataBorrowed || r.metadataStore != store || r.Owners.Components.Budget != manager || r.Owners.Components.Metadata == nil || r.Owners.PromotionRecovery != nil || r.Owners.PeerPromotionRecovery != nil || r.Owners.PeerPromotionCompletion != nil {
		return authenticatedstate.ErrSealIneligible
	}
	return nil
}
