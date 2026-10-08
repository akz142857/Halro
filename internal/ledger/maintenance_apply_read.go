package ledger

import (
	"context"
	"errors"
	"sync"
)

type acceptedApplyReadKey struct{}
type acceptedApplyReadScope struct {
	mu     sync.Mutex
	lease  *MaintenanceLease
	ctx    context.Context
	anchor ReplayAnchor
	closed bool
}

func (s *acceptedApplyReadScope) close() { s.mu.Lock(); s.closed = true; s.mu.Unlock() }

// ReadAcceptedApplyHead borrows only the current, already-durable AppendApply
// callback's frozen native head. The opaque context scope is created by that
// actual native mutation, and revoked/joined before its callback can return.
// found=false means this context has no such evidence; caller must use an
// independent frozen-read job instead. A stale/wrong scope never falls back.
// This avoids reentering the maintenance mutation gate from a derived Apply.
// It proves a source boundary, not the caller's payload; consumers must still
// authenticate native bytes and compare the complete Record. Neither this
// scope nor a raw hash establishes HA confirmation or Runtime role authority.
// visit must not Close/reenter the lease. Its context must not escape.
func (m *MaintenanceLease) ReadAcceptedApplyHead(ctx context.Context, visit func(context.Context, Watermark, [32]byte) error) (found bool, resultErr error) {
	scope, ok := ctx.Value(acceptedApplyReadKey{}).(*acceptedApplyReadScope)
	if !ok {
		return false, nil
	}
	if m == nil || visit == nil || scope == nil || scope.lease != m {
		return true, ErrLedgerMaintenance
	}
	if !scope.mu.TryLock() {
		return true, ErrLedgerMaintenance
	}
	defer scope.mu.Unlock()
	if scope.closed {
		return true, ErrLedgerMaintenance
	}
	if err := errors.Join(ctx.Err(), scope.ctx.Err()); err != nil {
		return true, err
	}
	if _, err := m.ValidateOwner(m.log); err != nil {
		return true, err
	}
	head, hash, valid := m.log.ChainHead()
	if !valid || head != scope.anchor.Head || hash != scope.anchor.Hash {
		return true, ErrLedgerMaintenance
	}
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(scope.ctx, cancel)
	defer func() { stop(); cancel() }()
	err := visit(opCtx, head, hash)
	actual, actualHash, actualValid := m.log.ChainHead()
	if !actualValid || actual != head || actualHash != hash {
		err = errors.Join(err, ErrLedgerMaintenance)
	}
	return true, errors.Join(err, opCtx.Err(), scope.ctx.Err())
}
