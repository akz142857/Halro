package app

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

func (f *haSelectedRuntimeFoundation) businessHealthError(ctx context.Context) error {
	if f == nil || f.ctx.Err() != nil || !f.mu.TryLock() {
		return authenticatedstate.ErrRecovery
	}
	reopened, audits, cause := f.reopened, f.audits, errors.Join(f.applyErr, f.closeErr)
	f.mu.Unlock()
	if cause != nil || reopened == nil {
		return errors.Join(authenticatedstate.ErrRecovery, cause)
	}
	for _, audit := range audits {
		if audit == nil {
			return authenticatedstate.ErrRecovery
		}
		p := audit.Snapshot()
		if !p.Complete || !p.Ready || p.Err != nil {
			return errors.Join(authenticatedstate.ErrRecovery, p.Err)
		}
	}
	return errors.Join(reopened.BusinessHealthError(ctx), f.ctx.Err())
}
