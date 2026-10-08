package statecut

import (
	"context"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
)

// AdvanceFullHAPrefix reduces the saved seven-root generation to the actual
// Member-permitted cut when all four physical sources end at that cut. Primary
// permission is its durable local index; Replica permission is its applied
// index. A received/unapplied physical suffix requires the separate shrinking
// prefix protocol and is refused here before any component changes. Common
// publication and Primary confirmation remain separate, mandatory barriers.
func (r *ReopenedHA) AdvanceFullHAPrefix(ctx context.Context) (resultErr error) {
	if r == nil || r.ctx == nil || !r.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer r.mu.Unlock()
	if r.closed || r.advanceErr != nil || r.metadataSource == nil || r.Derivative == nil || r.Owners.Components.NativeLedger == nil || r.Owners.Components.Lease == nil || r.Owners.Components.Ledger == nil || r.Owners.Components.Usage == nil || r.Owners.Components.NativeGovernance == nil || r.Owners.Components.Governance == nil || r.Owners.Components.Audit == nil || r.Owners.Components.Directory == nil {
		return errors.Join(authenticatedstate.ErrClosed, r.advanceErr)
	}
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.ctx, cancel)
	defer func() { stop(); cancel() }()
	stage := "independent Member/Ordering/four-source permission"
	changed := false
	defer func() {
		if resultErr != nil {
			resultErr = fmt.Errorf("HA full-prefix advance %s: %w", stage, resultErr)
			if changed {
				r.advanceErr = resultErr
			}
		}
	}()
	return WithHASelectedCut(opCtx, r.Owners, func(ctx context.Context, cut HASelectedCut) error {
		if cut.Seal != r.Seal {
			return authenticatedstate.ErrSealIneligible
		}
		c := r.Owners.Components
		head, hash, _ := c.NativeLedger.ChainHead()
		if _, err := c.Lease.ValidateOwner(c.NativeLedger); err != nil {
			return err
		}
		return func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
			if head.Generation != cut.Allowed[0].Generation || head.Sequence != cut.Allowed[0].Sequence || hash != cut.Heads[0] {
				return authenticatedstate.ErrSealIneligible
			}
			return c.NativeGovernance.WithFrozenHead(ctx, c.Budget, func(ctx context.Context, gov governance.Summary) error {
				if cut.Allowed[2].Generation != 1 || gov.Records != cut.Allowed[2].Sequence || gov.LastHash != cut.Heads[2] {
					return authenticatedstate.ErrSealIneligible
				}
				return c.Audit.WithFrozenHead(ctx, c.Budget, func(ctx context.Context, auditHead audit.Summary) error {
					if cut.Allowed[1].Generation != 1 || auditHead.Records != cut.Allowed[1].Sequence || auditHead.LastHash != cut.Heads[1] {
						return authenticatedstate.ErrSealIneligible
					}
					nativeMetadata, err := r.metadataSource.EpochHead()
					if err != nil || nativeMetadata.Epoch != cut.Allowed[3].Generation || nativeMetadata.Sequence != cut.Allowed[3].Sequence || nativeMetadata.Hash != cut.Heads[3] {
						return errors.Join(authenticatedstate.ErrSealIneligible, err)
					}
					// Every physical/source predicate is checked before the first mutation.
					stage = "selected Metadata compound owner handoff"
					changed = true
					if !r.metadataBorrowed {
						if err := r.metadataSource.ResumeHASelectedRoot(ctx, c.Metadata, c.Publisher, c.Guard, cut.Authority); err != nil {
							return err
						}
						r.Owners.Components.Metadata = r.metadataSource
						r.metadataBorrowed = true
					}
					stage = "actual permitted Ledger native reduction"
					if _, err := c.Ledger.AdvanceFromNative(ctx, c.NativeLedger, ledger.ProjectionPrefix{Head: head, Hash: hash}); err != nil {
						return err
					}
					stage = "actual permitted Usage native reduction"
					if _, err := r.Derivative.CatchUpAuthenticatedFromNative(ctx, c.NativeLedger, c.Lease, c.Budget); err != nil {
						return err
					}
					stage = "actual permitted Governance native reduction"
					if _, err := c.Governance.AdvanceAuthenticatedFromNative(ctx, c.NativeGovernance, gov, c.Budget); err != nil {
						return err
					}
					stage = "actual permitted Metadata native reduction"
					if _, err := r.metadataSource.ReplayTo(ctx, nativeMetadata, nativeMetadata.Sequence); err != nil {
						return err
					}
					stage = "actual permitted Ledger directory reduction"
					if _, err := c.Directory.AdvanceFromNative(ctx, c.NativeLedger, c.Lease, c.Ledger); err != nil {
						return err
					}
					// This is a component barrier only. Its positions are checked again by
					// PublishHA against the actual independent authority before any Ready.
					lr, err := c.Ledger.SealReference(ctx)
					if err != nil || lr.Anchor.Head != head || lr.Anchor.Hash != hash {
						return errors.Join(authenticatedstate.ErrSealIneligible, err)
					}
					mr, err := r.metadataSource.SealReference(ctx)
					if err != nil || mr.Position.Epoch != nativeMetadata.Epoch || mr.Position.Sequence != nativeMetadata.Sequence || mr.Position.Offset != uint64(nativeMetadata.Offset) || mr.Position.Hash != nativeMetadata.Hash {
						return errors.Join(authenticatedstate.ErrSealIneligible, err)
					}
					if head.Offset < 0 || nativeMetadata.Offset < 0 {
						return authenticatedstate.ErrSealIneligible
					}
					return ctx.Err()
				})
			})
		}(ctx, head, hash)
	})
}
