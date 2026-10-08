package statecut

import (
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/authenticatedstate"
)

// ResumeSelectedMetadata returns the fully authenticated saved Metadata root
// to the actual joined native source owner under current independent HA
// permission. Other saved roots stay unchanged. This grants no services and
// publishes no new common seal; allowed suffix reduction is a separate step.
func (r *ReopenedHA) ResumeSelectedMetadata(ctx context.Context) error {
	if r == nil || r.metadataSource == nil || r.ctx == nil || !r.mu.TryLock() {
		return authenticatedstate.ErrSealIneligible
	}
	defer r.mu.Unlock()
	if r.closed || r.advanceErr != nil || r.metadataBorrowed || r.Owners.Components.Metadata == nil {
		return authenticatedstate.ErrClosed
	}
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.ctx, cancel)
	defer func() { stop(); cancel() }()
	return WithHASelectedCut(opCtx, r.Owners, func(ctx context.Context, cut HASelectedCut) error {
		if cut.Seal != r.Seal {
			return authenticatedstate.ErrSealIneligible
		}
		c := r.Owners.Components
		if err := r.metadataSource.ResumeHASelectedRoot(ctx, c.Metadata, c.Publisher, c.Guard, cut.Authority); err != nil {
			return err
		}
		ref, err := r.metadataSource.SealReference(ctx)
		if err != nil || ref.Root != r.Seal.Roots[1] || ref.Position != r.Seal.Positions[1] {
			return errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		r.Owners.Components.Metadata = r.metadataSource
		r.metadataBorrowed = true
		return ctx.Err()
	})
}
