package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// AuthenticatedHAPhysicalUsageSource deliberately has different method names
// from the Standalone extension. Only a live constructor-installed Member and
// Ordering scope can lend it. Physical source bytes do not authorize projection
// beyond that scope's independently permitted prefix.
type AuthenticatedHAPhysicalUsageSource interface {
	AuthenticatedUsageSource
	HAPhysicalNativeReference(context.Context) (authenticatedstate.PhysicalNativeReference, error)
	HAPhysicalNativePreparedEnd(context.Context) (int64, error)
	VerifyHAPhysicalNativePrefix(context.Context, authenticatedstate.PhysicalNativeReference, int64) error
	HAPhysicalSelectionDigest(context.Context) ([32]byte, error)
	ValidateHAPhysicalPublisher(context.Context, *authenticatedstate.SealCandidatePublisher) error
	VerifyHAPhysicalNoDebt(context.Context) error
	VerifyHAPhysicalNoDebtAfterCommon(context.Context) error
	VerifyHAReplicaPhysicalSource(context.Context) error
}

type haPhysicalUsageSource struct {
	AuthenticatedUsageSource
	source *authenticatedUsageSource
	scope  *haSelectedStartupScope
	key    any
}

func (p *AuthenticatedLedgerProjection) WithHASelectedPhysicalFrozenUsageSource(ctx context.Context, native *Log, manager *resourcebudget.Manager, scope [32]byte, visit func(context.Context, AuthenticatedHAPhysicalUsageSource) error) error {
	if visit == nil {
		return ErrProjectionSource
	}
	return p.WithHASelectedFrozenUsageSource(ctx, native, manager, scope, haPhysicalUsageVisitor(visit))
}

func (p *AuthenticatedLedgerProjection) WithHASelectedPhysicalMaintenanceUsageSource(ctx context.Context, native *Log, lease *MaintenanceLease, manager *resourcebudget.Manager, scope [32]byte, visit func(context.Context, AuthenticatedHAPhysicalUsageSource) error) error {
	if visit == nil {
		return ErrProjectionSource
	}
	return p.WithHASelectedMaintenanceUsageSource(ctx, native, lease, manager, scope, haPhysicalUsageVisitor(visit))
}

func haPhysicalUsageVisitor(visit func(context.Context, AuthenticatedHAPhysicalUsageSource) error) func(context.Context, AuthenticatedUsageSource) error {
	return haPhysicalUsageVisitorMode(visit, true)
}

// Only the actual HA preparation constructor may enter the source before its
// native physical owner exists. Exported read consumers require that owner.
func haPhysicalUsageVisitorMode(visit func(context.Context, AuthenticatedHAPhysicalUsageSource) error, requirePhysical bool) func(context.Context, AuthenticatedUsageSource) error {
	return func(ctx context.Context, source AuthenticatedUsageSource) error {
		s, ok := source.(*authenticatedUsageSource)
		if !ok {
			return ErrProjectionSource
		}
		var key any = haSelectedLiveScopeKey{}
		scope, _ := ctx.Value(key).(*haSelectedStartupScope)
		if scope == nil {
			key = haSelectedStartupScopeKey{}
			scope, _ = ctx.Value(key).(*haSelectedStartupScope)
		}
		loan := &haPhysicalUsageSource{AuthenticatedUsageSource: source, source: s, scope: scope, key: key}
		s.mu.Lock()
		err := loan.checkLocked(ctx, requirePhysical)
		s.mu.Unlock()
		if err != nil {
			return err
		}
		return visit(ctx, loan)
	}
}

// source.mu and the original projection/native frozen source lane are held.
// Full==allowed is required by NoDebt below, not by ordinary physical reads:
// an actual Replica can have an authentic received suffix beyond Applied.
func (q *haPhysicalUsageSource) checkLocked(ctx context.Context, requirePhysical bool) error {
	if q == nil || q.source == nil || q.scope == nil {
		return ErrProjectionSource
	}
	s := q.source
	if err := s.check(ctx); err != nil {
		return err
	}
	n, p := s.native, s.projection
	scope, _ := ctx.Value(q.key).(*haSelectedStartupScope)
	if n == nil || p == nil || scope != q.scope || !scope.active.Load() || n.startupProvenance == nil || scope.owner != n.startupProvenance.ha || scope.authority.Role != scope.owner.role || scope.authority.Coverage != authenticatedstate.SealCoverageAuthenticated || !validHAUsageSourceMode(n, nil, scope.owner, scope.authority) || p.selectedNative.Load() != n || p.budget != n.startupBudget || !n.selectedStartupStructuralAllowedLocked() {
		return ErrProjectionSource
	}
	allowed := scope.allowed
	if allowed.Generation == 0 || (s.head.Head.Generation < allowed.Generation || s.head.Head.Generation == allowed.Generation && s.head.Head.Sequence < allowed.Sequence) || (s.ref.Anchor.Head.Generation > allowed.Generation || s.ref.Anchor.Head.Generation == allowed.Generation && s.ref.Anchor.Head.Sequence > allowed.Sequence) || s.ref.Anchor.Head.Generation == allowed.Generation && s.ref.Anchor.Head.Sequence == allowed.Sequence && s.ref.Anchor.Hash != allowed.Hash || s.head.Head.Generation == allowed.Generation && s.head.Head.Sequence == allowed.Sequence && s.head.Hash != allowed.Hash {
		return ErrProjectionSource
	}
	if !requirePhysical {
		return nil
	}
	o := n.physical.Load()
	if o == nil || o.projection != p || o.failure != nil {
		return authenticatedstate.ErrRecovery
	}
	return n.checkPhysicalNativeLocked()
}

func (q *haPhysicalUsageSource) HAPhysicalNativeReference(ctx context.Context) (authenticatedstate.PhysicalNativeReference, error) {
	s := q.source
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := q.checkLocked(ctx, true); err != nil {
		return authenticatedstate.PhysicalNativeReference{}, err
	}
	return s.native.physicalNativeReferenceLocked()
}

// Read-only consumer qualification under the original source/native loan.
// Equal prepared extents cannot make a Primary's bank into Replica credit.
func (q *haPhysicalUsageSource) VerifyHAReplicaPhysicalSource(ctx context.Context) error {
	if q == nil || q.source == nil {
		return ErrProjectionSource
	}
	s := q.source
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := q.checkLocked(ctx, true); err != nil {
		return err
	}
	if !s.native.options.Replica || q.scope.authority.Role != authenticatedstate.SealReplica {
		return ErrProjectionSource
	}
	return ctx.Err()
}
func (q *haPhysicalUsageSource) HAPhysicalNativePreparedEnd(ctx context.Context) (int64, error) {
	s := q.source
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := q.checkLocked(ctx, true); err != nil {
		return 0, err
	}
	n, o := s.native, s.native.physical.Load()
	var rangeErr error
	if o.allocatedEnd > n.offset {
		rangeErr = resourcebudget.CheckPhysicalRange(n.file, n.offset, o.allocatedEnd)
	}
	if err := errors.Join(rangeErr, resourcebudget.CheckPhysicalTail(n.file, n.offset, o.allocatedEnd)); err != nil {
		return 0, fmt.Errorf("HA native prepared coverage EOF=%d end=%d range=%v: %w", n.offset, o.allocatedEnd, rangeErr, err)
	}
	return o.allocatedEnd, nil
}
func (q *haPhysicalUsageSource) VerifyHAPhysicalNativePrefix(ctx context.Context, prior authenticatedstate.PhysicalNativeReference, end int64) error {
	s := q.source
	s.replayMu.Lock()
	defer s.replayMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := q.checkLocked(ctx, true); err != nil {
		return err
	}
	current, err := s.native.physicalNativeReferenceLocked()
	if err != nil {
		return err
	}
	return directoryVerifyPhysicalPrefixLocked(ctx, s.native, current, prior, end)
}
func (q *haPhysicalUsageSource) ValidateHAPhysicalPublisher(ctx context.Context, publisher *authenticatedstate.SealCandidatePublisher) error {
	s := q.source
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := q.checkLocked(ctx, true); err != nil {
		return err
	}
	p := s.native.startupProvenance
	if publisher == nil || publisher != p.publisher {
		return ErrProjectionSource
	}
	return publisher.ValidateOwner(p.guard, s.projection.budget)
}
func (q *haPhysicalUsageSource) commonLocked(ctx context.Context) (authenticatedstate.CandidateSeal, [32]byte, error) {
	return q.commonQualifiedLocked(ctx, true)
}

func (q *haPhysicalUsageSource) commonQualifiedLocked(ctx context.Context, requirePhysical bool) (authenticatedstate.CandidateSeal, [32]byte, error) {
	if err := q.checkLocked(ctx, requirePhysical); err != nil {
		return authenticatedstate.CandidateSeal{}, [32]byte{}, err
	}
	s := q.source
	p := s.native.startupProvenance
	if err := p.publisher.ValidateOwner(p.guard, s.projection.budget); err != nil {
		return authenticatedstate.CandidateSeal{}, [32]byte{}, err
	}
	seal, found, err := p.publisher.CurrentContext(ctx)
	if err != nil || !found {
		return seal, [32]byte{}, errors.Join(ErrProjectionSource, err)
	}
	if err = authenticatedstate.ValidateSealEligibility(seal, q.scope.authority); err != nil {
		return seal, [32]byte{}, err
	}
	raw, err := json.Marshal(seal)
	if err != nil {
		return seal, [32]byte{}, err
	}
	return seal, sha256.Sum256(raw), nil
}
func (q *haPhysicalUsageSource) HAPhysicalSelectionDigest(ctx context.Context) ([32]byte, error) {
	s := q.source
	s.mu.Lock()
	defer s.mu.Unlock()
	_, digest, err := q.commonLocked(ctx)
	if err == nil && digest != s.native.physical.Load().selection {
		err = ErrProjectionSource
	}
	return digest, err
}
func (q *haPhysicalUsageSource) VerifyHAPhysicalNoDebt(ctx context.Context) error {
	return q.verifyNoDebt(ctx, true)
}

// Post-common retirement is a separate authority lane. The actual new common
// may already be durable while captured old receipt banks retain its previous
// selection; this never authorizes ordinary Enable or a stale live scope.
func (q *haPhysicalUsageSource) VerifyHAPhysicalNoDebtAfterCommon(ctx context.Context) error {
	if _, ok := q.key.(haSelectedStartupScopeKey); !ok {
		return ErrProjectionSource
	}
	return q.verifyNoDebt(ctx, false)
}

func (q *haPhysicalUsageSource) verifyNoDebt(ctx context.Context, requireReceiptSelection bool) error {
	if q == nil || q.source == nil {
		return ErrProjectionSource
	}
	s := q.source
	s.mu.Lock()
	defer s.mu.Unlock()
	seal, digest, err := q.commonLocked(ctx)
	if err != nil {
		return err
	}
	o := s.native.physical.Load()
	if requireReceiptSelection && (!o.commonQualified || digest != o.selection) || o.debt != 0 || o.active != 0 {
		return authenticatedstate.ErrRecovery
	}
	return q.sourceNoDebtLocked(ctx, seal)
}

// This proof uses the actual original pin and the independently allowed tuple;
// it cannot reenter projection/native locks or borrow a candidate-header cut.
func (q *haPhysicalUsageSource) sourceNoDebtLocked(ctx context.Context, seal authenticatedstate.CandidateSeal) error {
	s := q.source
	allowed := q.scope.allowed
	if s.ref.Anchor != s.head || s.head.Head.Generation != allowed.Generation || s.head.Head.Sequence != allowed.Sequence || s.head.Hash != allowed.Hash {
		return authenticatedstate.ErrRecovery
	}
	rootFound, positionFound := false, false
	for _, root := range seal.Roots[:seal.RootCount] {
		rootFound = rootFound || root == s.ref.Root
	}
	for _, pos := range seal.Positions[:seal.PositionCount] {
		positionFound = positionFound || pos.Component == authenticatedstate.SealComponentLedger && pos.Generation == s.head.Head.Generation && pos.Sequence == s.head.Head.Sequence && pos.Offset == uint64(s.head.Head.Offset) && pos.Hash == s.head.Hash && pos.Epoch == uint64(s.head.Epoch)
	}
	if !rootFound || !positionFound {
		return authenticatedstate.ErrRecovery
	}
	for _, bucket := range []string{"pending", "active_requests"} {
		lo, hi, err := recoveryBounds(bucket, nil)
		if err != nil {
			return err
		}
		page, err := s.pin.Range(ctx, authenticatedstate.RangeOptions{Lo: lo, Hi: hi, MaxRows: 1, MaxBytes: 8 << 20, MaxPages: 4096})
		if err != nil {
			return s.remember(err)
		}
		if len(page.Rows) != 0 || !page.Done {
			return authenticatedstate.ErrRecovery
		}
	}
	return q.checkLocked(ctx, false)
}
