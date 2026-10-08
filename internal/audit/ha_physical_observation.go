package audit

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/nativephysical"
)

// ObserveHAReplicaPhysicalNative authenticates the actual native prefix and
// original receipt. It grants no receive, confirmation or common-cut authority.
func (l *Log) ObserveHAReplicaPhysicalNative(ctx context.Context) (out nativephysical.PreparedObservation, resultErr error) {
	if l == nil || !l.mu.TryLock() {
		return out, nativephysical.ErrBusy
	}
	defer l.mu.Unlock()
	if !l.replica || l.startupProvenance == nil || l.startupProvenance.ha == nil || l.physical == nil || l.file == nil || l.closed || l.closing || l.terminalErr != nil {
		return out, ErrStartupCache
	}
	head := l.physicalHeadLocked()
	if err := l.authenticatePhysicalPrefixLocked(ctx, head); err != nil {
		return out, err
	}
	out, resultErr = l.physical.ObservePrepared(ctx, head)
	resultErr = errors.Join(resultErr, ctx.Err())
	if resultErr != nil {
		out = nativephysical.PreparedObservation{}
	}
	return out, resultErr
}
