package app

import (
	"context"
	"errors"
	"sync"
)

type adminAuditPause struct {
	self    *adminAuditPause
	owner   *adminAuditBatchOwner
	mu      sync.Mutex
	resumed bool
}

// This pause belongs to the original finite batch owner. It rejects new enqueue
// before encoding and joins its actual native/checkpoint worker before returning.
// Cancellation preserves the installed pause; it never pretends that IO exited.
func (r *Runtime) pauseAdminAuditBatches(ctx context.Context) (func() error, error) {
	if r == nil || ctx == nil || ctx.Err() != nil {
		return nil, errors.Join(errAdminAuditBatchUnavailable, adminAuditContextError(ctx))
	}
	owner := &r.auditBatches
	owner.mu.Lock()
	if owner.retired || owner.paused != nil {
		owner.mu.Unlock()
		return nil, errAdminAuditBatchUnavailable
	}
	p := &adminAuditPause{owner: owner}
	p.self = p
	owner.paused = p
	done, running := owner.done, owner.running
	owner.mu.Unlock()
	resume := func() error {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.self != p || p.resumed {
			return errAdminAuditBatchUnavailable
		}
		owner.mu.Lock()
		defer owner.mu.Unlock()
		if owner.paused != p || owner.retired || owner.running {
			return errAdminAuditBatchUnavailable
		}
		owner.paused = nil
		p.resumed = true
		return nil
	}
	if running {
		select {
		case <-done:
		case <-ctx.Done():
			return resume, ctx.Err()
		}
	}
	owner.mu.Lock()
	err := owner.closeErr
	retired := owner.retired
	owner.mu.Unlock()
	if retired {
		err = errors.Join(err, errAdminAuditBatchUnavailable)
	}
	return resume, err
}
func adminAuditContextError(ctx context.Context) error {
	if ctx == nil {
		return errAdminAuditBatchUnavailable
	}
	return ctx.Err()
}
