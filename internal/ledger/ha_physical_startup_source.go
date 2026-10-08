package ledger

import (
	"context"
	"errors"
	"io"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// SelectedHAPhysicalStartupSource carries only the actual constructor's full
// HA startup freeze. It grants no Standalone permission and no projection cut
// from a receipt/header. VerifyAnchors still qualifies the independent Allowed
// prefix before a domain can restore its own exact COW root.
type SelectedHAPhysicalStartupSource interface {
	SelectedStartupSource
	HAPhysicalNativeReference(context.Context) (authenticatedstate.PhysicalNativeReference, error)
	VerifyHAPhysicalNativePrefix(context.Context, authenticatedstate.PhysicalNativeReference, int64) error
	HAPhysicalNativePreparedEnd(context.Context) (int64, error)
}

type haPhysicalStartupSource struct {
	SelectedStartupSource
	source *selectedStartupSource
	scope  *haSelectedStartupScope
}

func (l *Log) WithHASelectedPhysicalStartupOwner(ctx context.Context, budget *resourcebudget.Manager, options SelectedLedgerProjectionOpenOptions, identity [32]byte, visit func(context.Context, SelectedHAPhysicalStartupSource) error) error {
	if visit == nil {
		return ErrProjectionSource
	}
	return l.WithHASelectedStartupOwner(ctx, budget, options, identity, func(ctx context.Context, source SelectedStartupSource) error {
		s, ok := source.(*selectedStartupSource)
		if !ok {
			return ErrProjectionSource
		}
		scope, _ := ctx.Value(haSelectedStartupScopeKey{}).(*haSelectedStartupScope)
		q := &haPhysicalStartupSource{SelectedStartupSource: source, source: s, scope: scope}
		s.mu.Lock()
		err := q.checkLocked(ctx)
		s.mu.Unlock()
		if err != nil {
			return err
		}
		return visit(ctx, q)
	})
}

func (q *haPhysicalStartupSource) checkLocked(ctx context.Context) error {
	if q == nil || q.source == nil || q.scope == nil {
		return ErrProjectionSource
	}
	s := q.source
	if err := s.checkLocked(ctx); err != nil {
		return err
	}
	n := s.native
	scope, _ := ctx.Value(haSelectedStartupScopeKey{}).(*haSelectedStartupScope)
	if n.startupProvenance == nil || n.startupProvenance.ha == nil || scope != q.scope || scope.owner != n.startupProvenance.ha || !scope.active.Load() || s.seal.Role != scope.owner.role || !validHAUsageSourceMode(n, nil, scope.owner, scope.authority) || !n.selectedStartupStructuralAllowedLocked() {
		return ErrProjectionSource
	}
	return authenticatedstate.ValidateSealEligibility(s.seal, scope.authority)
}
func (q *haPhysicalStartupSource) HAPhysicalNativeReference(ctx context.Context) (authenticatedstate.PhysicalNativeReference, error) {
	s := q.source
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := q.checkLocked(ctx); err != nil {
		return authenticatedstate.PhysicalNativeReference{}, err
	}
	return s.native.physicalNativeReferenceLocked()
}
func (q *haPhysicalStartupSource) VerifyHAPhysicalNativePrefix(ctx context.Context, prior authenticatedstate.PhysicalNativeReference, end int64) error {
	s := q.source
	s.replayMu.Lock()
	defer s.replayMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := q.checkLocked(ctx); err != nil {
		return err
	}
	n := s.native
	actual, err := n.physicalNativeReferenceLocked()
	if err != nil {
		return err
	}
	if prior.File != actual.File || prior.Parent != actual.Parent || prior.Generation != actual.Generation || prior.EOF < 0 || prior.EOF > actual.EOF || prior.Sequence > actual.Sequence || end < actual.EOF {
		return ErrProjectionSource
	}
	verifier := &chainVerifier{key: n.chainKey, hash: n.chainAnchor, sequence: n.startSequence}
	head, partial, err := scan(io.NewSectionReader(n.file, 0, prior.EOF), n.generation, 0, n.startSequence, func(Record) error { return ctx.Err() }, verifier)
	if err != nil || partial || head.Offset != prior.EOF || head.Sequence != prior.Sequence || verifier.hash != prior.Hash {
		return errors.Join(ErrProjectionSource, err)
	}
	return errors.Join(resourcebudget.CheckPhysicalTail(n.file, actual.EOF, end), q.checkLocked(ctx))
}
func (q *haPhysicalStartupSource) HAPhysicalNativePreparedEnd(ctx context.Context) (int64, error) {
	s := q.source
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := q.checkLocked(ctx); err != nil {
		return 0, err
	}
	n := s.native
	o := n.physical.Load()
	if o == nil || o.failure != nil || o.projection == nil || o.projection.selectedNative.Load() != n || o.projection.budget != n.startupBudget || o.allocatedEnd < n.offset {
		return 0, authenticatedstate.ErrRecovery
	}
	if err := n.checkPhysicalNativeLocked(); err != nil {
		return 0, err
	}
	var rangeErr error
	if o.allocatedEnd > n.offset {
		rangeErr = resourcebudget.CheckPhysicalRange(n.file, n.offset, o.allocatedEnd)
	}
	return o.allocatedEnd, errors.Join(rangeErr, resourcebudget.CheckPhysicalTail(n.file, n.offset, o.allocatedEnd), q.checkLocked(ctx))
}
