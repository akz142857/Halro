package ledger

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Covers the complete point-read job, including raw bytes, JSON decoding and
// validation. Snapshot queries have a separate fee in the same Manager. This
// logical floor needs physical calibration; callers retaining returned values
// must cover their own lifetime. It does not authorize saved-root startup.
const AuthenticatedLedgerReadRAMBytes int64 = 64 << 20

var _ AccountingLeaseReader = (*AuthenticatedLedgerProjection)(nil)

type authenticatedLedgerReadFault struct{ err error }

func (p *AuthenticatedLedgerProjection) readFault() error {
	if fault := p.readFailure.Load(); fault != nil {
		return errors.Join(ErrProjectionCorrupt, fault.err)
	}
	return nil
}

func authenticatedPointJob(ctx context.Context, p *AuthenticatedLedgerProjection, visit func(context.Context, *authenticatedstate.Snapshot, ReplayAnchor) error) error {
	return authenticatedReadJob(ctx, p, AuthenticatedLedgerReadRAMBytes, visit)
}

func authenticatedReadJob(ctx context.Context, p *AuthenticatedLedgerProjection, ram int64, visit func(context.Context, *authenticatedstate.Snapshot, ReplayAnchor) error) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil {
		return ErrProjectionUnavailable
	}
	if p.businessReads.Load() {
		release, err := p.admitBusinessRead(ctx)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, release()) }()
		return p.withHASelectedBusinessReadLocked(ctx, func(ctx context.Context, allowed *HASelectedLedgerPosition) error {
			return authenticatedReadJobLocked(ctx, p, ram, func(ctx context.Context, pin *authenticatedstate.Snapshot, anchor ReplayAnchor) error {
				if allowed != nil && (anchor.Head.Generation > allowed.Generation || anchor.Head.Generation == allowed.Generation && (anchor.Head.Sequence > allowed.Sequence || anchor.Head.Sequence == allowed.Sequence && anchor.Hash != allowed.Hash)) {
					return ErrProjectionSource
				}
				return visit(ctx, pin, anchor)
			})
		})
	}
	if !p.mu.TryRLock() {
		return authenticatedstate.ErrBusy
	}
	defer p.mu.RUnlock()
	return authenticatedReadJobLocked(ctx, p, ram, visit)
}

// Caller holds the owner lock through physical snapshot Close and fee release.
// Advance also uses this helper while holding the exclusive owner lock.
func authenticatedPointJobLocked(ctx context.Context, p *AuthenticatedLedgerProjection, visit func(context.Context, *authenticatedstate.Snapshot, ReplayAnchor) error) error {
	return authenticatedReadJobLocked(ctx, p, AuthenticatedLedgerReadRAMBytes, visit)
}

func authenticatedReadJobLocked(ctx context.Context, p *AuthenticatedLedgerProjection, ram int64, visit func(context.Context, *authenticatedstate.Snapshot, ReplayAnchor) error) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.selectedGrantError(ctx, false); err != nil {
		return err
	}
	if !p.ready || p.readFailure.Load() != nil || p.closeContext.Err() != nil {
		return errors.Join(ErrProjectionUnavailable, p.failure, p.readFault())
	}
	permit, err := p.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: ram})
	if err != nil {
		return err
	}
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.closeContext, cancel)
	var pin *authenticatedstate.Snapshot
	returned := false
	defer func() {
		var cleanupErr error
		if pin != nil {
			cleanupErr = pin.Close()
		}
		// AfterFunc is asynchronous: test the owner synchronously as well.
		resultErr = errors.Join(resultErr, cleanupErr, ctx.Err(), opCtx.Err(), p.closeContext.Err(), p.readFault(), p.selectedGrantError(ctx, false))
		stop()
		cancel()
		var feeErr error
		if errors.Is(resultErr, ownedfile.ErrRetirement) || errors.Is(resultErr, ErrSegmentRetirement) {
			resultErr = finishAuthenticatedSourceCredit(permit, resultErr)
		} else {
			feeErr = permit.Release()
			resultErr = errors.Join(resultErr, feeErr)
		}
		if !returned || cleanupErr != nil || feeErr != nil || errors.Is(resultErr, ErrProjectionCorrupt) || errors.Is(resultErr, authenticatedstate.ErrCorrupt) {
			p.readFailure.CompareAndSwap(nil, &authenticatedLedgerReadFault{err: resultErr})
		}
	}()
	pin, resultErr = p.store.Snapshot(opCtx)
	if resultErr == nil {
		var position projectionPosition
		var found bool
		position, found, resultErr = authenticatedPointGet[projectionPosition](opCtx, pin, "meta", []byte("position"))
		if resultErr == nil && !found {
			resultErr = ErrProjectionCorrupt
		}
		if resultErr == nil {
			resultErr = visit(opCtx, pin, ReplayAnchor{Head: position.Watermark, Hash: position.Hash, Epoch: position.Epoch})
		}
	}
	returned = true
	return resultErr
}

func authenticatedPointGet[T any](ctx context.Context, pin *authenticatedstate.Snapshot, bucket string, key []byte) (value T, found bool, resultErr error) {
	k, err := authenticatedLedgerKey(bucket, key)
	if err != nil {
		return value, false, err
	}
	raw, found, err := pin.Get(ctx, k)
	if err != nil || !found {
		return value, false, err
	}
	value, err = decodeProjectionValue[T](raw)
	if err != nil {
		var zero T
		return zero, false, err
	}
	return value, true, nil
}

func authenticatedPointRead[T any](ctx context.Context, p *AuthenticatedLedgerProjection, bucket string, key []byte, validate func(T, ReplayAnchor) error) (value T, found bool, resultErr error) {
	resultErr = authenticatedPointJob(ctx, p, func(c context.Context, pin *authenticatedstate.Snapshot, head ReplayAnchor) error {
		var err error
		value, found, err = authenticatedPointGet[T](c, pin, bucket, key)
		if err == nil && found && validate != nil {
			err = validate(value, head)
		}
		return err
	})
	if resultErr != nil {
		var zero T
		return zero, false, resultErr
	}
	return value, found, nil
}

func authenticatedLedgerReadLocked[T any](ctx context.Context, p *AuthenticatedLedgerProjection, bucket string, key []byte) (value T, found bool, resultErr error) {
	resultErr = authenticatedPointJobLocked(ctx, p, func(c context.Context, pin *authenticatedstate.Snapshot, _ ReplayAnchor) error {
		var err error
		value, found, err = authenticatedPointGet[T](c, pin, bucket, key)
		return err
	})
	if resultErr != nil {
		var zero T
		return zero, false, resultErr
	}
	return value, found, nil
}

func (p *AuthenticatedLedgerProjection) ReadReplayAnchor(ctx context.Context) (anchor ReplayAnchor, err error) {
	err = authenticatedPointJob(ctx, p, func(_ context.Context, _ *authenticatedstate.Snapshot, head ReplayAnchor) error {
		anchor = head
		return nil
	})
	if err != nil {
		return ReplayAnchor{}, err
	}
	return anchor, nil
}
func (p *AuthenticatedLedgerProjection) ReadPosition(ctx context.Context) (Watermark, [32]byte, error) {
	a, err := p.ReadReplayAnchor(ctx)
	return a.Head, a.Hash, err
}
func (p *AuthenticatedLedgerProjection) ReadBalance(ctx context.Context, key BalanceKey) (Balance, error) {
	v, _, err := authenticatedPointRead(ctx, p, "balances", projectionBalanceKey(key), func(v Balance, _ ReplayAnchor) error {
		if v.CommittedMicrosUSD < 0 || v.ReservedMicrosUSD < 0 || v.InputTokens < 0 || v.OutputTokens < 0 || v.UnknownAttempts < 0 {
			return ErrProjectionCorrupt
		}
		return nil
	})
	return v, err
}
func authenticatedPointID(id string) error {
	if id == "" || len(id) > maxPayloadSize {
		return ErrProjectionPageLimit
	}
	return nil
}
func (p *AuthenticatedLedgerProjection) ReadLease(ctx context.Context, id string) (Record, bool, error) {
	if err := authenticatedPointID(id); err != nil {
		return Record{}, false, err
	}
	return authenticatedPointRead(ctx, p, "leases", projectionIDKey(id), func(v Record, head ReplayAnchor) error {
		return validateProjectedLease(v, id, head.Head)
	})
}
func (p *AuthenticatedLedgerProjection) ReadSettlement(ctx context.Context, id string) (SettledAttempt, bool, error) {
	if err := authenticatedPointID(id); err != nil {
		return SettledAttempt{}, false, err
	}
	return authenticatedPointRead(ctx, p, "settled", projectionIDKey(id), func(v SettledAttempt, _ ReplayAnchor) error {
		return validateProjectedSettlement(v, id)
	})
}
func authenticatedRequestValid(v ProjectedRequest, id string, head ReplayAnchor) error {
	return validateProjectedRequest(v, id, head.Head)
}
func (p *AuthenticatedLedgerProjection) ReadRequest(ctx context.Context, id string) (ProjectedRequest, bool, error) {
	if err := authenticatedPointID(id); err != nil {
		return ProjectedRequest{}, false, err
	}
	return authenticatedPointRead(ctx, p, "requests", projectionIDKey(id), func(v ProjectedRequest, head ReplayAnchor) error { return authenticatedRequestValid(v, id, head) })
}
func (p *AuthenticatedLedgerProjection) ReadFinalization(ctx context.Context, id string) (Record, bool, error) {
	v, found, err := p.ReadRequest(ctx, id)
	if err != nil || !found || v.Finalized == nil {
		return Record{}, false, err
	}
	return *v.Finalized, true, nil
}
func (p *AuthenticatedLedgerProjection) ReadLegacyRequest(ctx context.Context, id string) (bool, error) {
	v, found, err := p.ReadRequest(ctx, id)
	return found && v.Legacy(), err
}
func (p *AuthenticatedLedgerProjection) ReadRun(ctx context.Context, id string) (domain.Run, bool, error) {
	if err := authenticatedPointID(id); err != nil {
		return domain.Run{}, false, err
	}
	return authenticatedPointRead(ctx, p, "runs", projectionIDKey(id), func(v domain.Run, _ ReplayAnchor) error {
		if v.ID != id || v.Validate() != nil {
			return ErrProjectionCorrupt
		}
		return nil
	})
}
func (p *AuthenticatedLedgerProjection) ReadWorkUnit(ctx context.Context, id string) (domain.WorkUnit, bool, error) {
	if err := authenticatedPointID(id); err != nil {
		return domain.WorkUnit{}, false, err
	}
	return authenticatedPointRead(ctx, p, "work_units", projectionIDKey(id), func(v domain.WorkUnit, _ ReplayAnchor) error {
		if v.ID != id || v.Validate() != nil {
			return ErrProjectionCorrupt
		}
		return nil
	})
}
func (p *AuthenticatedLedgerProjection) ReadGovernanceIdempotency(ctx context.Context, projectID, operation, keyHash string) (GovernanceIdempotency, bool, error) {
	for _, id := range []string{projectID, operation, keyHash} {
		if err := authenticatedPointID(id); err != nil {
			return GovernanceIdempotency{}, false, err
		}
	}
	return authenticatedPointRead(ctx, p, "idempotency", projectionTuple(projectID, operation, keyHash), func(v GovernanceIdempotency, _ ReplayAnchor) error {
		return validateProjectedGovernanceIdempotency(v, projectID, operation, keyHash)
	})
}
func (p *AuthenticatedLedgerProjection) ReadEventPosition(ctx context.Context, id string) (first Watermark, found bool, resultErr error) {
	if err := authenticatedPointID(id); err != nil {
		return first, false, err
	}
	resultErr = authenticatedPointJob(ctx, p, func(c context.Context, pin *authenticatedstate.Snapshot, head ReplayAnchor) error {
		var err error
		first, found, err = authenticatedPointGet[Watermark](c, pin, "event_positions", projectionIDKey(id))
		if err != nil {
			return err
		}
		_, hasDigest, err := authenticatedPointGet[[32]byte](c, pin, "events", projectionIDKey(id))
		if err != nil {
			return err
		}
		if found != hasDigest || found && validateProjectionRecordPosition(first, head.Head) != nil {
			return ErrProjectionCorrupt
		}
		return nil
	})
	if resultErr != nil {
		return Watermark{}, false, resultErr
	}
	return first, found, nil
}
