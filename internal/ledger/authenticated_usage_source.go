package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// AuthenticatedUsageSource supplies native facts and Ledger dependencies from
// one exact live owner. It exists only inside WithFrozenUsageSource. This is a
// local source boundary, not an HA permission or a reusable startup token.
type AuthenticatedUsageSource interface {
	Check(context.Context) error
	Reference() AuthenticatedLedgerSealReference
	NativeHead() ReplayAnchor
	ReplayAuthenticated(context.Context, ReplayAnchor, func(Record) error) (Watermark, error)
	VerifyAnchors(context.Context, [2]ReplayAnchor) error
	ReadFinalization(context.Context, string) (Record, bool, error)
	ReadLegacyRequest(context.Context, string) (bool, error)
	ReadEventPosition(context.Context, string) (Watermark, bool, error)
}

type authenticatedUsageSource struct {
	mu              sync.Mutex
	replayMu        sync.Mutex
	active          bool
	ctx             context.Context
	native          *Log
	projection      *AuthenticatedLedgerProjection
	pin             *authenticatedstate.Snapshot
	ref             AuthenticatedLedgerSealReference
	head            ReplayAnchor
	failure         error
	completeHistory bool
}

type usageSourceDebt struct {
	error
	credit resourcebudget.Permit
}

func (e *usageSourceDebt) Unwrap() error { return e.error }

// WithFrozenUsageSource joins a normal native read job after startup maintenance
// has been released. It pins the actual Ledger owner before freezing the actual
// native writer, in the same lock order as durable Apply. Appends may wait for
// this finite read job; callbacks must not reenter/Close owner or native. Close
// of either actual owner cancels and joins the whole callback. There is no fake
// maintenance lease, no raw-hash authorization and no snapshot of a live bolt DB.
func (p *AuthenticatedLedgerProjection) WithFrozenUsageSource(ctx context.Context, native *Log, manager *resourcebudget.Manager, expectedLedgerScope [32]byte, visit func(context.Context, AuthenticatedUsageSource) error) error {
	return p.withFrozenUsageSource(ctx, native, nil, manager, expectedLedgerScope, visit)
}

// WithSelectedMaintenanceUsageSource lends the same actual non-structural
// selected root/native source while startup still owns its original lease.
// It does not grant whole-history completion or common publication authority.
func (p *AuthenticatedLedgerProjection) WithSelectedMaintenanceUsageSource(ctx context.Context, native *Log, lease *MaintenanceLease, manager *resourcebudget.Manager, expectedLedgerScope [32]byte, visit func(context.Context, AuthenticatedUsageSource) error) error {
	if p == nil || native == nil || p.selectedNative.Load() != native {
		return ErrProjectionSource
	}
	if replicated, err := lease.ValidateOwner(native); err != nil || replicated {
		return errors.Join(ErrProjectionSource, err)
	}
	if err := native.ValidateSelectedStartupRead(ctx); err != nil {
		return err
	}
	return p.withFrozenUsageSource(ctx, native, lease, manager, expectedLedgerScope, visit)
}

func (p *AuthenticatedLedgerProjection) withFrozenUsageSource(ctx context.Context, native *Log, lease *MaintenanceLease, manager *resourcebudget.Manager, expectedLedgerScope [32]byte, visit func(context.Context, AuthenticatedUsageSource) error) error {
	return p.withFrozenUsageSourceMode(ctx, native, lease, manager, expectedLedgerScope, visit, false)
}

func (p *AuthenticatedLedgerProjection) withFrozenUsageSourceMode(ctx context.Context, native *Log, lease *MaintenanceLease, manager *resourcebudget.Manager, expectedLedgerScope [32]byte, visit func(context.Context, AuthenticatedUsageSource) error, haReadOnly bool) error {
	return p.withFrozenUsageSourcePermissions(ctx, native, lease, manager, expectedLedgerScope, visit, haReadOnly, false)
}

func (p *AuthenticatedLedgerProjection) withFrozenUsageSourcePermissions(ctx context.Context, native *Log, lease *MaintenanceLease, manager *resourcebudget.Manager, expectedLedgerScope [32]byte, visit func(context.Context, AuthenticatedUsageSource) error, haReadOnly, completeHistory bool) error {
	return p.withFrozenUsageSourcePermissionsMode(ctx, native, lease, manager, expectedLedgerScope, visit, haReadOnly, completeHistory, false)
}

func (p *AuthenticatedLedgerProjection) withFrozenUsageSourcePermissionsMode(ctx context.Context, native *Log, lease *MaintenanceLease, manager *resourcebudget.Manager, expectedLedgerScope [32]byte, visit func(context.Context, AuthenticatedUsageSource) error, haReadOnly, completeHistory, haSelected bool) error {
	if p == nil || native == nil || visit == nil {
		return ErrProjectionSource
	}
	finish, err := native.beginMutation(lease)
	if err != nil {
		return err
	}
	defer finish()
	return p.withFrozenRootReference(ctx, manager, expectedLedgerScope, completeHistory, func(ctx context.Context, ref AuthenticatedLedgerSealReference) (resultErr error) {
		credit, err := manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: AuthenticatedLedgerSourceRAMBytes})
		if err != nil {
			return err
		}
		defer func() {
			if !errors.Is(resultErr, ownedfile.ErrRetirement) && !errors.Is(resultErr, ErrSegmentRetirement) {
				if err := credit.Release(); err == nil {
					return
				} else {
					resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, err)
				}
			}
			// The actual component retirement fault owns this whole job's credit.
			resultErr = &usageSourceDebt{error: resultErr, credit: credit}
		}()
		opCtx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(native.closeContext, cancel)
		defer func() { stop(); cancel() }()
		native.mu.Lock()
		defer native.mu.Unlock()
		// Historical audit descriptor lookups briefly hold this same mutex.
		// Validate under the source job's existing serialization instead of a
		// preliminary TryLock that turns that ordinary overlap into startup failure.
		if err := errors.Join(opCtx.Err(), native.closeContext.Err(), p.checkNativeWriterLocked(native)); err != nil {
			return err
		}
		if completeHistory && (p.selectedNative.Load() != native || native.startupProvenance == nil || native.startupBudget != manager || !native.selectedStartupStructuralAllowedLocked()) {
			return ErrStartupAuditPending
		}
		if audit := native.startupAuditPtr.Load(); audit != nil {
			stopAudit := audit.OnWithdrawal(cancel)
			defer stopAudit()
		}
		if native.file == nil || native.closeContext.Err() != nil || native.status.Load() != AccountingHealthy || !haSelected && (native.options.Replica && !haReadOnly || haReadOnly && (lease == nil || !lease.readOnly || !native.options.Replica) || native.options.AfterDurable != nil || native.options.BeforeDurable != nil || native.options.WaitConfirmed != nil) || ref.Anchor.Head.After(Watermark{Generation: native.generation, Sequence: native.sequence, Offset: native.offset}) {
			return ErrProjectionSource
		}
		open := func(opCtx context.Context) (resultErr error) {
			pin, err := p.store.Snapshot(opCtx)
			if err != nil {
				return err
			}
			s := &authenticatedUsageSource{active: true, ctx: opCtx, native: native, projection: p, pin: pin, ref: ref, completeHistory: completeHistory, head: ReplayAnchor{Head: Watermark{Generation: native.generation, Sequence: native.sequence, Offset: native.offset}, Hash: native.chainHash, Epoch: native.chainEpoch}}
			defer func() {
				// Escaped replay/read calls must exit before the native writer resumes
				// and before the root pin can retire. No caller can reactivate s.
				s.replayMu.Lock()
				defer s.replayMu.Unlock()
				s.mu.Lock()
				defer s.mu.Unlock()
				s.active = false
				resultErr = errors.Join(resultErr, s.failure, pin.Close(), opCtx.Err(), native.closeContext.Err())
				if s.failure != nil {
					p.readFailure.CompareAndSwap(nil, &authenticatedLedgerReadFault{err: s.failure})
				}
				s.pin, s.native = nil, nil
			}()
			if err := s.Check(opCtx); err != nil {
				return err
			}
			return visit(opCtx, s)
		}
		if !haSelected {
			return open(opCtx)
		}
		if p.selectedNative.Load() != native || native.startupProvenance == nil || native.startupProvenance.ha == nil || native.startupBudget != manager {
			return ErrProjectionSource
		}
		h := native.startupProvenance.ha
		permission := h.withLiveScope
		if scope, ok := opCtx.Value(haSelectedStartupScopeKey{}).(*haSelectedStartupScope); ok && scope.owner == h && scope.active.Load() {
			permission = h.withScope
		}
		return permission(opCtx, func(ctx context.Context, a authenticatedstate.SealEligibility, allowed HASelectedLedgerPosition) error {
			if !validHAUsageSourceMode(native, lease, h, a) || allowed.Generation == 0 || ref.Anchor.Head.Generation > allowed.Generation || ref.Anchor.Head.Generation == allowed.Generation && ref.Anchor.Head.Sequence > allowed.Sequence || native.generation < allowed.Generation || native.generation == allowed.Generation && native.sequence < allowed.Sequence {
				return ErrProjectionSource
			}
			if ref.Anchor.Head.Generation == allowed.Generation && ref.Anchor.Head.Sequence == allowed.Sequence && ref.Anchor.Hash != allowed.Hash {
				return ErrProjectionSource
			}
			if native.generation == allowed.Generation && native.sequence == allowed.Sequence && native.chainHash != allowed.Hash {
				return ErrProjectionSource
			}
			return open(ctx)
		})
	})
}

func (s *authenticatedUsageSource) Reference() AuthenticatedLedgerSealReference { return s.ref }
func (s *authenticatedUsageSource) NativeHead() ReplayAnchor                    { return s.head }
func (s *authenticatedUsageSource) Check(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.check(ctx)
}
func (s *authenticatedUsageSource) check(ctx context.Context) error {
	if !s.active || s.pin == nil || s.native == nil {
		return ErrProjectionUnavailable
	}
	return errors.Join(ctx.Err(), s.ctx.Err(), s.native.closeContext.Err(), s.failure, s.native.validateSelectedStartupGrant(ctx, s.completeHistory))
}

func (s *authenticatedUsageSource) remember(err error) error {
	if errors.Is(err, ErrProjectionCorrupt) || errors.Is(err, authenticatedstate.ErrCorrupt) || errors.Is(err, ownedfile.ErrRetirement) {
		s.failure = errors.Join(s.failure, err)
	}
	return err
}
func (s *authenticatedUsageSource) ReplayAuthenticated(ctx context.Context, from ReplayAnchor, visit func(Record) error) (Watermark, error) {
	if !s.replayMu.TryLock() {
		return Watermark{}, authenticatedstate.ErrBusy
	}
	defer s.replayMu.Unlock()
	s.mu.Lock()
	err := s.check(ctx)
	native := s.native
	s.mu.Unlock()
	if err != nil {
		return Watermark{}, err
	}
	c, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	if native.startupProvenance != nil && !s.completeHistory && projectionPrefixBefore(from.Head, native.startupSaved.Head) {
		return Watermark{}, ErrProjectionCursor
	}
	head, err := native.replayAuthenticatedLocked(c, from, 0, visit, nil)
	s.mu.Lock()
	if native.status.Load() == AccountingRecoveryRequired {
		s.failure = errors.Join(s.failure, ErrProjectionCorrupt, err)
	}
	s.mu.Unlock()
	return head, errors.Join(err, s.Check(c))
}

// VerifyAnchors authenticates the actual source from its independently selected
// saved cut, or from genesis for ordinary full-history owners. The pair covers
// saved Usage and selected
// Ledger dependencies; offsets/hashes supplied by the consumer are never proof.
func (s *authenticatedUsageSource) VerifyAnchors(ctx context.Context, anchors [2]ReplayAnchor) error {
	if !s.replayMu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer s.replayMu.Unlock()
	s.mu.Lock()
	err := s.check(ctx)
	native := s.native
	s.mu.Unlock()
	if err != nil {
		return err
	}
	c, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	var seen [2]bool
	observe := func(a ReplayAnchor) {
		for i := range anchors {
			seen[i] = seen[i] || a == anchors[i]
		}
	}
	from := ReplayAnchor{}
	if native.startupProvenance != nil && !s.completeHistory {
		from = native.startupSaved
		for _, a := range anchors {
			if projectionPrefixBefore(a.Head, from.Head) {
				return ErrProjectionCursor
			}
		}
	}
	observe(from)
	head, err := native.replayAuthenticatedLocked(c, from, 0, func(r Record) error {
		observe(ReplayAnchor{Head: Watermark{Generation: r.Generation, Offset: r.Offset, Sequence: r.Sequence}, Hash: r.Hash, Epoch: r.Epoch})
		return c.Err()
	}, func(a ReplayAnchor) error { observe(a); return c.Err() })
	s.mu.Lock()
	if native.status.Load() == AccountingRecoveryRequired {
		s.failure = errors.Join(s.failure, ErrProjectionCorrupt, err)
	}
	s.mu.Unlock()
	if err != nil || head != s.head.Head || !seen[0] || !seen[1] {
		return fmt.Errorf("source boundary verification (head matches=%t anchors=%t/%t): %w", head == s.head.Head, seen[0], seen[1], errors.Join(ErrProjectionSource, err, c.Err()))
	}
	return s.Check(c)
}
func (s *authenticatedUsageSource) readRequest(ctx context.Context, id string) (ProjectedRequest, bool, error) {
	if err := authenticatedPointID(id); err != nil {
		return ProjectedRequest{}, false, err
	}
	if err := s.check(ctx); err != nil {
		return ProjectedRequest{}, false, err
	}
	v, found, err := authenticatedPointGet[ProjectedRequest](ctx, s.pin, "requests", projectionIDKey(id))
	if err == nil && found {
		err = authenticatedRequestValid(v, id, s.ref.Anchor)
	}
	return v, found, s.remember(err)
}
func (s *authenticatedUsageSource) ReadFinalization(ctx context.Context, id string) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, found, err := s.readRequest(ctx, id)
	if err != nil || !found || v.Finalized == nil {
		return Record{}, false, err
	}
	return *v.Finalized, true, nil
}
func (s *authenticatedUsageSource) ReadLegacyRequest(ctx context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, found, err := s.readRequest(ctx, id)
	return found && v.Legacy(), err
}
func (s *authenticatedUsageSource) ReadEventPosition(ctx context.Context, id string) (Watermark, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := errors.Join(authenticatedPointID(id), s.check(ctx)); err != nil {
		return Watermark{}, false, err
	}
	v, found, err := authenticatedPointGet[Watermark](ctx, s.pin, "event_positions", projectionIDKey(id))
	_, digest, digestErr := authenticatedPointGet[[32]byte](ctx, s.pin, "events", projectionIDKey(id))
	if err = errors.Join(err, digestErr); err != nil {
		return Watermark{}, false, s.remember(err)
	}
	if found != digest || found && (v.Sequence == 0 || v.Sequence > s.ref.Anchor.Head.Sequence || v.Generation == 0 || v.Offset <= 0) {
		return Watermark{}, false, s.remember(ErrProjectionCorrupt)
	}
	return v, found, nil
}

// ValidateStandaloneReadSource checks actual live FD/name and source lifecycle
// for already-qualified derivative reads. It does not authenticate old bytes
// or authorize root selection. A source job holding mu uses its opaque Check.
func (l *Log) ValidateStandaloneReadSource(ctx context.Context) error {
	if l == nil {
		return ErrProjectionSource
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := errors.Join(ctx.Err(), l.closeContext.Err()); err != nil {
		return err
	}
	if l.file == nil || len(l.chainKey) != 32 || l.status.Load() != AccountingHealthy || l.options.Replica || l.options.AfterDurable != nil || l.options.BeforeDurable != nil || l.options.WaitConfirmed != nil || !l.selectedStartupAppendAllowedLocked() {
		return ErrProjectionSource
	}
	actual, err := l.file.Stat()
	named, nameErr := os.Lstat(l.path)
	if err != nil || nameErr != nil || named.Mode()&os.ModeSymlink != 0 || !os.SameFile(actual, named) || actual.Size() < l.offset {
		l.status.RequireRecovery()
		return errors.Join(ErrCorrupt, err, nameErr)
	}
	return ctx.Err()
}
