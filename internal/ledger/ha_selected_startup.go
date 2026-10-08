package ledger

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// WithAuthority must freeze the actual Member, Ordering and four native sources
// through the complete visitor. A saved Seal or caller position is insufficient.
// HASelectedLedgerPosition comes from the independently allowed source cut.
// No file offset supplied by a caller is accepted as source authority.
type HASelectedLedgerPosition struct {
	Generation, Sequence uint64
	Hash                 [32]byte
}

type HASelectedSealOpenOptions struct {
	SelectedSealOpenOptions
	WithAuthority     func(context.Context, func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error) error
	WithLiveAuthority func(context.Context, func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error) error
}

type haSelectedStartupProvenance struct {
	withAuthority     func(context.Context, func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error) error
	withLiveAuthority func(context.Context, func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error) error
	role              authenticatedstate.SealRole
}
type haSelectedStartupScopeKey struct{}
type haSelectedLiveScopeKey struct{}
type haSelectedMaintenanceScopeKey struct{}
type haSelectedMaintenanceScope struct {
	lease  *MaintenanceLease
	active atomic.Bool
}

func (m *MaintenanceLease) borrowsHASelectedMaintenance(ctx context.Context) bool {
	s, ok := ctx.Value(haSelectedMaintenanceScopeKey{}).(*haSelectedMaintenanceScope)
	return m != nil && ok && s.lease == m && s.active.Load() && ctx.Err() == nil
}

type haSelectedStartupScope struct {
	owner     *haSelectedStartupProvenance
	authority authenticatedstate.SealEligibility
	allowed   HASelectedLedgerPosition
	active    atomic.Bool
	mu        sync.Mutex
	joined    *sync.Cond
	borrowers int
	closing   bool
}

func (s *haSelectedStartupScope) borrow(ctx context.Context) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || !s.active.Load() || ctx.Err() != nil || s.borrowers >= 8 {
		return nil, errors.Join(ErrProjectionSource, ctx.Err())
	}
	s.borrowers++
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.borrowers--
		s.joined.Broadcast()
	}, nil
}

func (s *haSelectedStartupScope) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closing = true
	for s.borrowers != 0 {
		s.joined.Wait()
	}
	s.active.Store(false)
}

func (h *haSelectedStartupProvenance) withScope(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error) error {
	if h == nil {
		return ErrProjectionSource
	}
	return h.withRoleScope(ctx, haSelectedStartupScopeKey{}, h.withAuthority, visit)
}

// A live single-Ledger read authority can never mint the startup/common-cut
// context capability, even when both callbacks have matching literal fields.
func (h *haSelectedStartupProvenance) withLiveScope(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error) error {
	if h == nil {
		return ErrProjectionSource
	}
	return h.withRoleScope(ctx, haSelectedLiveScopeKey{}, h.withLiveAuthority, visit)
}

func (h *haSelectedStartupProvenance) withRoleScope(ctx context.Context, key any, provider func(context.Context, func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error) error, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error) error {
	if provider == nil || visit == nil {
		return ErrProjectionSource
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrProjectionSource, err)
	}
	parentMaintenance, _ := ctx.Value(haSelectedMaintenanceScopeKey{}).(*haSelectedMaintenanceScope)
	var otherKey any = haSelectedStartupScopeKey{}
	if _, startup := key.(haSelectedStartupScopeKey); startup {
		otherKey = haSelectedLiveScopeKey{}
	}
	if scope, ok := ctx.Value(otherKey).(*haSelectedStartupScope); ok && scope.owner == h {
		// The single-source live permission and full frozen startup permission
		// have different lock and authority contracts. Neither can recursively
		// acquire the other's Member freeze or upgrade the other's capability.
		return ErrProjectionSource
	}
	// Only this constructor's private, still-live scope can borrow an existing
	// actual Member/source freeze. Re-entering that freeze would deadlock; a
	// caller-supplied SealEligibility is never a substitute for this capability.
	if scope, ok := ctx.Value(key).(*haSelectedStartupScope); ok && scope.owner == h {
		finish, err := scope.borrow(ctx)
		if err != nil {
			return err
		}
		defer finish()
		return visit(ctx, scope.authority, scope.allowed)
	}
	var entered atomic.Bool
	var callbackMu sync.Mutex
	var callbackErr error
	record := func(err error) error {
		callbackMu.Lock()
		callbackErr = errors.Join(callbackErr, err)
		callbackMu.Unlock()
		return err
	}
	err := provider(ctx, func(ctx context.Context, authority authenticatedstate.SealEligibility, allowed HASelectedLedgerPosition) error {
		if !entered.CompareAndSwap(false, true) || authority.Role != h.role || authority.Coverage != authenticatedstate.SealCoverageAuthenticated {
			return record(ErrProjectionSource)
		}
		if err := ctx.Err(); err != nil {
			return record(err)
		}
		if parentMaintenance != nil {
			actualMaintenance, _ := ctx.Value(haSelectedMaintenanceScopeKey{}).(*haSelectedMaintenanceScope)
			if actualMaintenance != parentMaintenance || !parentMaintenance.active.Load() {
				return record(ErrProjectionSource)
			}
		}
		scope := &haSelectedStartupScope{owner: h, authority: authority, allowed: allowed}
		scope.joined = sync.NewCond(&scope.mu)
		scope.active.Store(true)
		defer scope.close()
		return record(visit(context.WithValue(ctx, key, scope), authority, allowed))
	})
	callbackMu.Lock()
	err = errors.Join(err, callbackErr, ctx.Err())
	callbackMu.Unlock()
	if !entered.Load() {
		return errors.Join(ErrProjectionSource, err)
	}
	return err
}

// WithHASelectedSealAuthority borrows the constructor's independent HA
// authority while the actual Member, Ordering and source owners are frozen.
// It acquires the actual maintenance gate before the Member freeze, and takes
// no native mutex. Derived owners can therefore
// join the same frozen cut without recursively acquiring the Member freeze.
// The context capability expires at callback return.
func (l *Log) WithHASelectedSealAuthority(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error) error {
	if l == nil || l.startupProvenance == nil || l.startupProvenance.ha == nil {
		return ErrProjectionSource
	}
	l.admission.Lock()
	lease := l.maintenance
	l.admission.Unlock()
	if lease == nil {
		return ErrLedgerMaintenance
	}
	if lease.borrowsHASelectedMaintenance(ctx) {
		return l.startupProvenance.ha.withScope(ctx, visit)
	}
	opCtx, release, err := lease.enter(ctx)
	if err != nil {
		return err
	}
	defer release()
	finish, err := l.beginMutation(lease)
	if err != nil {
		return err
	}
	defer finish()
	scope := &haSelectedMaintenanceScope{lease: lease}
	scope.active.Store(true)
	defer scope.active.Store(false)
	return l.startupProvenance.ha.withScope(context.WithValue(opCtx, haSelectedMaintenanceScopeKey{}, scope), visit)
}

func validateHASelectedStartupScope(ctx context.Context, h *haSelectedStartupProvenance, authority authenticatedstate.SealEligibility, options Options) error {
	s, ok := ctx.Value(haSelectedStartupScopeKey{}).(*haSelectedStartupScope)
	if !ok || s.owner != h || !s.active.Load() || s.authority != authority || authority.Role != h.role {
		return ErrProjectionSource
	}
	if h.role == authenticatedstate.SealReplica {
		if !options.Replica || options.BeforeDurable != nil || options.AfterDurable != nil || options.WaitConfirmed != nil || options.BeforeRoll != nil || options.AfterRoll != nil {
			return ErrProjectionSource
		}
	} else if h.role != authenticatedstate.SealPrimary || options.Replica || options.BeforeDurable == nil || options.AfterDurable == nil || options.WaitConfirmed == nil {
		return ErrProjectionSource
	}
	return ctx.Err()
}

// OpenWithHASelectedSeal preserves the actual primary hooks or replica mode.
// The physical tail is authenticated in full. Only the independently allowed
// prefix is loaned to derived owners; received/unapplied facts stay excluded.
func OpenWithHASelectedSeal(ctx context.Context, path string, status *Status, options Options, o HASelectedSealOpenOptions) (result *Log, receipt NativeStartupReceipt, resultErr error) {
	role := authenticatedstate.SealPrimary
	if options.Replica {
		role = authenticatedstate.SealReplica
	}
	h := &haSelectedStartupProvenance{withAuthority: o.WithAuthority, withLiveAuthority: o.WithLiveAuthority, role: role}
	resultErr = h.withScope(ctx, func(ctx context.Context, authority authenticatedstate.SealEligibility, allowed HASelectedLedgerPosition) error {
		if err := validateHASelectedStartupScope(ctx, h, authority, options); err != nil {
			return err
		}
		selected := o.SelectedSealOpenOptions
		selected.Authority = authority
		var err error
		result, receipt, err = openWithSelectedSeal(ctx, path, status, options, selected, h)
		if err != nil {
			return err
		}
		result.mu.Lock()
		receipt.Allowed, err = result.resolveHAAllowedAnchorLocked(ctx, allowed)
		result.mu.Unlock()
		if err != nil {
			return err
		}
		return ctx.Err()
	})
	if resultErr != nil && result != nil {
		resultErr = errors.Join(resultErr, result.Close())
		if !errors.Is(resultErr, ErrSegmentRetirement) && !errors.Is(resultErr, ownedfile.ErrRetirement) {
			result = nil
		}
		receipt = NativeStartupReceipt{}
	}
	return
}

// resolveHAAllowedAnchorLocked accepts no caller offset/epoch. It derives them
// from authentic native bytes, checks the whole physical source and exact
// independent tuple, and refuses an allowed cut below the selected saved root.
// The caller owns both the actual HA permission scope and native mutation lock.
func (l *Log) resolveHAAllowedAnchorLocked(ctx context.Context, allowed HASelectedLedgerPosition) (ReplayAnchor, error) {
	if l == nil || l.startupProvenance == nil || l.startupProvenance.ha == nil || allowed.Generation == 0 || l.file == nil {
		return ReplayAnchor{}, ErrProjectionSource
	}
	scope, ok := ctx.Value(haSelectedStartupScopeKey{}).(*haSelectedStartupScope)
	if !ok || scope.owner != l.startupProvenance.ha || !scope.active.Load() || scope.allowed != allowed {
		return ReplayAnchor{}, ErrProjectionSource
	}
	full := ReplayAnchor{Head: Watermark{Generation: l.generation, Sequence: l.sequence, Offset: l.offset}, Hash: l.chainHash, Epoch: l.chainEpoch}
	if allowed.Generation == full.Head.Generation && allowed.Sequence == full.Head.Sequence && allowed.Hash == full.Hash {
		return full, ctx.Err()
	}
	prefix, report, err := InspectProjectionCursor(ctx, l.path, l.chainKey, allowed.Generation, allowed.Sequence)
	if err != nil || prefix.Hash != allowed.Hash || report.Head != full.Head || report.ChainHash != full.Hash || report.ChainVerified != l.chainSawFrames || projectionPrefixBefore(prefix.Head, l.startupSaved.Head) {
		return ReplayAnchor{}, errors.Join(ErrProjectionSource, err)
	}
	var anchor ReplayAnchor
	seen := false
	observe := func(a ReplayAnchor) error {
		if a.Head == prefix.Head && a.Hash == prefix.Hash {
			anchor, seen = a, true
		}
		return ctx.Err()
	}
	_ = observe(l.startupSaved)
	head, err := l.replayAuthenticatedLocked(ctx, l.startupSaved, 0, func(r Record) error {
		return observe(ReplayAnchor{Head: Watermark{Generation: r.Generation, Sequence: r.Sequence, Offset: r.Offset}, Hash: r.Hash, Epoch: r.Epoch})
	}, observe)
	if err != nil || !seen || head != full.Head || l.chainHash != full.Hash {
		return ReplayAnchor{}, errors.Join(ErrProjectionSource, err)
	}
	return anchor, ctx.Err()
}

// The real HA freeze precedes the native/projection locks. Its context loan
// expires before the independent Member freeze exits; no escaped callback can
// reuse an old allowed cut. Ordinary Standalone entry points refuse HA scope.
func (l *Log) WithHASelectedStartupOwner(ctx context.Context, budget *resourcebudget.Manager, o SelectedLedgerProjectionOpenOptions, identity [32]byte, visit func(context.Context, SelectedStartupSource) error) error {
	if l == nil || l.startupProvenance == nil || l.startupProvenance.ha == nil {
		return ErrProjectionSource
	}
	return l.WithHASelectedSealAuthority(ctx, func(ctx context.Context, authority authenticatedstate.SealEligibility, allowed HASelectedLedgerPosition) error {
		o.Authority = authority
		return l.withSelectedStartupOwner(ctx, budget, o, identity, visit)
	})
}

func OpenHASelectedAuthenticatedLedgerProjection(ctx context.Context, native *Log, directory string, o AuthenticatedLedgerOpenOptions, selected SelectedLedgerProjectionOpenOptions) (result *AuthenticatedLedgerProjection, receipt AuthenticatedLedgerReopen, resultErr error) {
	if native == nil || native.startupProvenance == nil || native.startupProvenance.ha == nil {
		return nil, receipt, ErrProjectionSource
	}
	resultErr = native.WithHASelectedSealAuthority(ctx, func(ctx context.Context, authority authenticatedstate.SealEligibility, allowed HASelectedLedgerPosition) error {
		if o.Prefix == nil || o.Prefix.Head.Generation != allowed.Generation || o.Prefix.Head.Sequence != allowed.Sequence || o.Prefix.Hash != allowed.Hash {
			return ErrProjectionSource
		}
		selected.Authority = authority
		var err error
		result, receipt, err = openAuthenticatedLedgerProjectionQualified(ctx, native, directory, o, nil, &selected)
		return err
	})
	if resultErr != nil && result != nil {
		resultErr = errors.Join(resultErr, result.Close())
		if !errors.Is(resultErr, ownedfile.ErrRetirement) && !errors.Is(resultErr, ErrSegmentRetirement) {
			result = nil
		}
		receipt = AuthenticatedLedgerReopen{}
	}
	return
}

// ValidateHASelectedNativeWriter requires the constructor's actual HA source
// freeze, a joined historical audit, the original Manager and a live exclusive
// primary maintenance lease. It establishes no durable confirmation or Ready.
func (p *AuthenticatedLedgerProjection) ValidateHASelectedNativeWriter(ctx context.Context, native *Log, lease *MaintenanceLease, manager *resourcebudget.Manager) error {
	if p == nil || native == nil || native.startupProvenance == nil || native.startupProvenance.ha == nil || native.startupProvenance.ha.role != authenticatedstate.SealPrimary || p.selectedNative.Load() != native || native.startupBudget != manager {
		return ErrProjectionSource
	}
	if replicated, err := lease.ValidateOwner(native); err != nil || !replicated || lease.readOnly {
		return errors.Join(ErrProjectionSource, err)
	}
	return native.WithHASelectedSealAuthority(ctx, func(ctx context.Context, authority authenticatedstate.SealEligibility, allowed HASelectedLedgerPosition) error {
		if err := p.ValidateResourceOwner(ctx, manager); err != nil {
			return err
		}
		if err := native.validateSelectedStartupGrant(ctx, true); err != nil {
			return err
		}
		return lease.WithHead(ctx, func(ctx context.Context, head Watermark, hash [32]byte) error {
			if head.Generation != allowed.Generation || head.Sequence != allowed.Sequence || hash != allowed.Hash {
				return ErrProjectionSource
			}
			if err := p.checkNativeWriter(native); err != nil {
				return err
			}
			actual, actualHash, err := p.ReadPosition(ctx)
			if err != nil || actual != head || actualHash != hash {
				return errors.Join(ErrProjectionSource, err)
			}
			return native.validateSelectedStartupGrant(ctx, true)
		})
	})
}
