package metadatastate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

// Only the selected HA factory installs this provenance. A maintenance root,
// facade permission, receipt, or candidate header cannot manufacture it.
type haPhysicalSource struct {
	publisher           *authenticatedstate.SealCandidatePublisher
	guard               *lock.Lock
	withAuthority       func(context.Context, func(context.Context, authenticatedstate.SealEligibility, HASelectedMetadataPosition, metadatajournal.BoundReplicationAuthority) error) error
	withConfirmedApply  func(context.Context, *Baseline, metadatajournal.Head, uint64, func(context.Context) error) error
	role                authenticatedstate.SealRole
	permission          metadatajournal.BoundReplicationAuthority
	receiveWindow       *TerminalPhysicalWindow
	receiveJobs         sync.WaitGroup
	closing             bool
	recoveryPermitted   metadatajournal.Head
	recoverySeal        authenticatedstate.CandidateSeal
	receiveRegistration *metadatajournal.BoundPhysicalReceiveRegistration
}

func (b *Baseline) withTerminalPhysicalNativeLocked(ctx context.Context, visit func(*metadatajournal.PhysicalNativeLoan) error) error {
	if s := b.haPhysicalSource; s != nil {
		if !b.physicalEnabled || s.publisher == nil {
			return ErrSource
		}
		return b.journal.WithBoundPhysicalNative(ctx, b.budget, s.guard, s.permission, s.role == authenticatedstate.SealReplica, visit)
	}
	return b.journal.WithPhysicalNative(ctx, b.budget, visit)
}

func (f *haFacade) PricePinTerminalContext(ctx context.Context) context.Context {
	return f.base.PricePinTerminalContext(ctx)
}

func (f *haFacade) PricePinRecoveryContext(ctx context.Context, count int) (context.Context, error) {
	if f.role != authenticatedstate.SealPrimary {
		return ctx, ErrSource
	}
	return f.base.PricePinRecoveryContext(ctx, count)
}
func (f *haFacade) FinishPricePinRecovery(ctx context.Context) error {
	if f.role != authenticatedstate.SealPrimary {
		return ErrSource
	}
	return f.base.FinishPricePinRecovery(ctx)
}

func (b *Baseline) withTerminalPhysicalRecoveryNativeLocked(ctx context.Context, visit func(*metadatajournal.PhysicalNativeLoan) error) error {
	if s := b.haPhysicalSource; s != nil {
		return b.journal.WithBoundPhysicalNativeRecovery(ctx, b.budget, s.guard, s.permission, s.role == authenticatedstate.SealReplica, visit)
	}
	return b.journal.WithPhysicalNativeRecovery(ctx, b.budget, visit)
}

func (b *Baseline) haPhysicalCommonSelectionLocked(ctx context.Context) error {
	s := b.haPhysicalSource
	if s == nil {
		return ErrSource
	}
	actual, err := b.sealReferenceLockedMode(ctx, false)
	if err != nil {
		return err
	}
	selected, found, err := s.publisher.CurrentContext(ctx)
	if err != nil || !found || selected.Role != s.role {
		return errors.Join(ErrSource, err)
	}
	rootFound, positionFound := false, false
	for _, r := range selected.Roots[:selected.RootCount] {
		if r == actual.Root {
			rootFound = true
		}
	}
	for _, p := range selected.Positions[:selected.PositionCount] {
		if p == actual.Position {
			positionFound = true
		}
	}
	if !rootFound || !positionFound {
		return ErrSource
	}
	// This is solely a Close refusal check. Exact signed common identity is
	// necessary; successful mutation/retirement still needs original HA scope.
	return s.publisher.ValidateOwner(s.guard, b.budget)
}

func (b *Baseline) withHAPhysicalAuthorityLocked(ctx context.Context, visit func(context.Context, authenticatedstate.CandidateSeal, *metadatajournal.PhysicalNativeLoan) error) error {
	s := b.haPhysicalSource
	if s == nil || b.haPeerTransition != nil || !b.ready.Load() || b.selectedStartup || b.readOnly || b.journal == nil || visit == nil {
		return ErrSource
	}
	if s.role == authenticatedstate.SealPrimary {
		if b.haPrimaryPrepared == nil || *b.haPrimaryPrepared != s.permission {
			return ErrSource
		}
	} else if s.role == authenticatedstate.SealReplica {
		if b.haReplicaPrepared == nil || *b.haReplicaPrepared != s.permission {
			return ErrSource
		}
	} else {
		return ErrSource
	}
	if err := s.publisher.ValidateOwner(s.guard, b.budget); err != nil {
		return err
	}
	var mu sync.Mutex
	active, entered := true, false
	var callbackErr error
	err := s.withAuthority(ctx, func(ctx context.Context, a authenticatedstate.SealEligibility, allowed HASelectedMetadataPosition, p metadatajournal.BoundReplicationAuthority) error {
		mu.Lock()
		defer mu.Unlock()
		if !active || entered || ctx == nil || a.Role != s.role || a.Instance != b.evidence.Identity || p != s.permission || allowed.Epoch != b.position.Epoch || allowed.Sequence < b.position.Sequence {
			callbackErr = errors.Join(callbackErr, ErrSource)
			return ErrSource
		}
		entered = true
		callbackErr = func() error {
			selected, found, err := s.publisher.SelectContext(ctx, a)
			if err != nil || !found {
				return errors.Join(ErrSource, err)
			}
			err = b.journal.WithBoundPhysicalNative(ctx, b.budget, s.guard, p, s.role == authenticatedstate.SealReplica, func(q *metadatajournal.PhysicalNativeLoan) error {
				h, _, err := q.HeadAt(ctx, allowed.Sequence)
				if err != nil || h.Hash != allowed.Hash || h.Epoch != allowed.Epoch {
					return errors.Join(ErrSource, err)
				}
				return visit(ctx, selected, q)
			})
			if err != nil {
				return err
			}
			final, found, err := s.publisher.SelectContext(ctx, a)
			if err != nil || !found || final != selected {
				return errors.Join(ErrSource, err)
			}
			return ctx.Err()
		}()
		return callbackErr
	})
	mu.Lock()
	defer mu.Unlock()
	active = false
	if !entered {
		return errors.Join(ErrSource, err)
	}
	return errors.Join(err, callbackErr)
}

// EnableHAPhysicalTerminals requires this owner's independently selected exact
// common root, original configured HA native writer, and full bound MAC source.
// It does not install a Standalone permission or promote an HA role.
func (b *Baseline) EnableHAPhysicalTerminals(ctx context.Context) error {
	if b == nil || !b.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	actual, err := b.sealReferenceLocked(ctx)
	if err != nil {
		return err
	}
	work, err := b.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: DeltaRAMBytes})
	if err != nil {
		return err
	}
	err = b.withHAPhysicalAuthorityLocked(ctx, func(ctx context.Context, seal authenticatedstate.CandidateSeal, q *metadatajournal.PhysicalNativeLoan) error {
		rootFound, positionFound := false, false
		for _, r := range seal.Roots[:seal.RootCount] {
			if r == actual.Root {
				rootFound = true
			}
		}
		for _, p := range seal.Positions[:seal.PositionCount] {
			if p == actual.Position {
				positionFound = true
			}
		}
		if !rootFound || !positionFound {
			return ErrSource
		}
		ref, err := q.Reference()
		if err != nil {
			return err
		}
		if ref.Sequence != b.position.Sequence || ref.Hash != b.position.Hash || ref.EOF != b.position.Offset {
			return ErrSource
		}
		if b.physicalEnabled {
			return nil
		}
		raw, err := json.Marshal(seal)
		if err != nil {
			return err
		}
		// The control owner outlives this source loan and includes actual Disk
		// charges. A borrowed RAM workspace cannot pay or retain that mixed
		// claim. Keep the original paid context for every native proof, but
		// acquire the persistent control fee from the same Manager normally.
		_, err = b.store.EnablePhysicalTerminalQualified(resourcebudget.WithoutRAMWorkspace(ctx), sha256.Sum256(raw), ref, func(prior authenticatedstate.PhysicalNativeReference, end int64) error {
			return q.VerifyPrefix(ctx, prior, end)
		})
		if err != nil {
			return err
		}
		if err := q.RetireUnused(); err != nil {
			return err
		}
		if err := q.Enable(); err != nil {
			return err
		}
		b.physicalEnabled = true
		return nil
	})
	if errors.Is(err, ownedfile.ErrRetirement) || errors.Is(err, ownedfile.ErrReplaced) || errors.Is(err, resourcebudget.ErrPhysicalAllocationUncertain) {
		b.sealDebt = work
		b.retirementErr = errors.Join(b.retirementErr, err)
		b.ready.Store(false)
		return err
	}
	return errors.Join(err, work.Release())
}
