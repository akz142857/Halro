package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

type selectedRotationPublisherFailure struct {
	publisher *authenticatedstate.SealCandidatePublisher
	disk      resourcebudget.Permit
	cause     error
}

func (e *selectedRotationPublisherFailure) Error() string { return e.cause.Error() }
func (e *selectedRotationPublisherFailure) Unwrap() error { return e.cause }

func retainSelectedRotationPublisherDebt(guard *lock.Lock, p *authenticatedstate.SealCandidatePublisher, disk resourcebudget.Permit, cause error) error {
	debt := &selectedRotationPublisherFailure{publisher: p, disk: disk, cause: cause}
	if err := guard.RetainCleanup(func() error { return debt }); err != nil {
		debt.cause = errors.Join(debt.cause, err)
	}
	return debt
}

func selectedRotationCurrent(ctx context.Context, cfg config.Config, i *selectedRotationIntent) (seal authenticatedstate.CandidateSeal, resultErr error) {
	scope := keyScope(ctx)
	guard, ok := scope.lock.(*lock.Lock)
	if !ok {
		return seal, authenticatedstate.ErrSealIneligible
	}
	m := scope.root.pressure.logicalBudget()
	sealDiskBytes := int64(32 << 10)
	if cfg.ResourceBudget != nil && cfg.ResourceBudget.PhysicalTerminalReserve {
		sealDiskBytes = authenticatedstate.PhysicalSealDiskBytes
	}
	disk, err := m.TryAcquireRecovery(resourcebudget.Cost{Disk: sealDiskBytes})
	if err != nil {
		return seal, err
	}
	var p *authenticatedstate.SealCandidatePublisher
	if cfg.ResourceBudget != nil && cfg.ResourceBudget.PhysicalTerminalReserve {
		// The signed intent supplies the independently captured source identity;
		// the actual publisher verifies its own receipt banks and original inodes.
		// This read does not enable physical publication or native readiness.
		p, err = authenticatedstate.OpenPhysicalSealCandidatePublisher(ctx, cfg.Storage.DataDir, guard, i.SourceBootstrap.LedgerKey, statecut.HardLimits(), m, disk, i.SourceBootstrap.Authority)
	} else {
		p, err = authenticatedstate.OpenSealCandidatePublisher(cfg.Storage.DataDir, guard, i.SourceBootstrap.LedgerKey, statecut.HardLimits(), m, disk)
	}
	if err != nil {
		if errors.Is(err, ownedfile.ErrRetirement) || errors.Is(err, authenticatedstate.ErrSealFileRetirement) {
			return seal, retainSelectedRotationPublisherDebt(guard, p, disk, err)
		}
		err = errors.Join(err, disk.Release())
		return seal, err
	}
	defer func() {
		if closeErr := p.Close(); closeErr != nil {
			resultErr = retainSelectedRotationPublisherDebt(guard, p, disk, errors.Join(resultErr, closeErr))
			return
		}
		resultErr = errors.Join(resultErr, disk.Release())
	}()
	current, found, err := p.Current()
	if err != nil || !found {
		return seal, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	if current == i.SourceSeal {
		return current, nil
	}
	authority := i.TargetBootstrap.Authority
	copy(authority.Positions[:], current.Positions[:])
	if err := authenticatedstate.ValidateSealEligibility(current, authority); err != nil {
		return seal, fmt.Errorf("current target eligibility: %w (current=%+v source=%+v)", err, current, i.SourceSeal)
	}
	if current.Generation != i.TargetSeal.Generation || current.Revision < i.TargetSeal.Revision || current.Positions[1].Epoch != i.TargetHead.Epoch {
		return seal, fmt.Errorf("current not authorized target: %w (current revision=%d source revision=%d current metadata=%+v source metadata=%+v target metadata=%+v)", authenticatedstate.ErrSealIneligible, current.Revision, i.SourceSeal.Revision, current.Positions[1], i.SourceSeal.Positions[1], i.TargetSeal.Positions[1])
	}
	return current, nil
}

func resumeSelectedRotationPublication(ctx context.Context, cfg config.Config, i *selectedRotationIntent, hook func(string) error) (resultErr error) {
	current, err := selectedRotationCurrent(ctx, cfg, i)
	if err != nil {
		return err
	}
	if current != i.SourceSeal {
		return nil
	}
	oldVault, err := vault.New(i.OldKey)
	if err != nil {
		return err
	}
	defer oldVault.Close()
	m := keyScope(ctx).root.pressure.logicalBudget()
	guard := keyScope(ctx).lock.(*lock.Lock)
	native, err := prepareCommandNativeDisk(ctx, cfg, guard)
	if err != nil {
		return err
	}
	var s *runtimeSelectedState
	defer func() {
		if s != nil {
			resultErr = errors.Join(resultErr, s.close())
		}
		resultErr = errors.Join(resultErr, native.finish(resultErr))
	}()
	if cfg.ResourceBudget.PhysicalTerminalReserve {
		ctx = selectedRotationColdSourceContext(ctx, i)
	}
	s, err = tryOpenSelectedRuntimeState(ctx, cfg, oldVault, guard, keyScope(ctx).root.pressure, native.growth, nil)
	if err != nil || s == nil {
		return fmt.Errorf("resume source open: %w", errors.Join(authenticatedstate.ErrSealIneligible, err))
	}
	if err := s.generation.WaitStartupAudit(ctx); err != nil {
		return err
	}
	if s.generation.Seal != i.SourceSeal {
		return fmt.Errorf("source reopen seal differs: %w", authenticatedstate.ErrSealIneligible)
	}
	if s.services != nil {
		s.services.Accounting.WithdrawAdmission()
		if err := errors.Join(s.services.Governance.Retire(ctx), s.services.Accounting.Retire(ctx), s.services.Metadata.RetireFacade(ctx)); err != nil {
			return err
		}
		if err := s.generation.DrainSelectedForOfflineRotation(ctx); err != nil {
			return err
		}
	}
	rootDisk, err := m.TryAcquireRecovery(resourcebudget.Cost{Disk: cfg.ResourceBudget.MetadataStoreDiskBytes})
	if err != nil {
		return err
	}
	journalDisk, err := m.TryAcquireRecovery(resourcebudget.Cost{Disk: cfg.ResourceBudget.MetadataJournalDiskBytes})
	if err != nil {
		return err
	}
	rootPath, journalPath := memoryStateMetadataPaths(cfg, i.TargetBootstrap.MetadataSlot)
	target, _, err := metadatastate.OpenFromRoot(ctx, i.SourceBootstrap.LedgerKey, rootPath, journalPath, metadatastate.OpenOptions{BaselineOptions: selectedRotationBaselineOptions(cfg, m, i.TargetBootstrap), Evidence: i.TargetBootstrap.Evidence, SavedRoot: authenticatedstate.RootIdentity{Scope: i.TargetRoot.Root.State.Scope, Cut: i.TargetRoot.Root.State.Cut, Digest: i.TargetRoot.Root.State.Digest, ID: i.TargetRoot.Root.State.PackID, Revision: i.TargetRoot.Root.State.Revision, Rows: i.TargetRoot.Root.Rows}, SavedHead: i.TargetHead, NativeHead: i.TargetHead, PermittedHead: i.TargetHead, RootDiskReservation: rootDisk, JournalDiskReservation: journalDisk, MaxJournalBytes: cfg.ResourceBudget.MetadataJournalDiskBytes})
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, target.Close()) }()
	ref, err := target.SealReference(ctx)
	if err != nil || ref != i.TargetRoot {
		return fmt.Errorf("resume candidate reference: %w (actual=%+v expected=%+v)", errors.Join(authenticatedstate.ErrSealIneligible, err), ref, i.TargetRoot)
	}
	owners := s.generation.Owners
	owners.Metadata = target
	authority, err := statecut.CurrentStandaloneAuthority(ctx, owners, i.TargetBootstrap.Authority)
	if err != nil {
		return err
	}
	caches, err := prepareSelectedCheckpointCaches(ctx, cfg, s)
	if err != nil {
		return err
	}
	if err := joinSelectedPhysicalRotationMetadata(ctx, cfg, s); err != nil {
		return err
	}
	_, err = statecut.PublishStandaloneAuthorizedRotation(ctx, owners, i.TargetSeal, authority, caches, func(ctx context.Context, seal authenticatedstate.CandidateSeal) error {
		if seal != i.TargetSeal {
			return fmt.Errorf("resume frozen seal differs: %w (actual=%+v expected=%+v)", authenticatedstate.ErrSealIneligible, seal, i.TargetSeal)
		}
		return callRotationHook(hook, "selected_before_seal")
	})
	if err != nil {
		return fmt.Errorf("resume target publication: %w", err)
	}
	if err := bindSelectedPhysicalRotationSuccessor(ctx, cfg, s, i.TargetSeal, authority); err != nil {
		return err
	}
	return callRotationHook(hook, "selected_after_seal")
}

type selectedRotationPressureKey struct{}
type selectedRotationPressureOwner struct {
	pressure *resourcePressureRuntime
	owner    *runtimeSelectedState
}

func startSelectedRotationPressure(ctx context.Context, cfg config.Config) (context.Context, func() error, error) {
	pressure := keyScope(ctx).root.pressure
	owner := &runtimeSelectedState{}
	if err := startSelectedPressureMonitor(ctx, cfg, pressure.logicalBudget(), pressure, owner); err != nil {
		if owner.pressureCancel != nil {
			owner.pressureCancel()
			<-owner.pressureDone
		}
		return ctx, nil, err
	}
	capability := &selectedRotationPressureOwner{pressure: pressure, owner: owner}
	return context.WithValue(ctx, selectedRotationPressureKey{}, capability), func() error {
		owner.pressureCancel()
		err := <-owner.pressureDone
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}, nil
}
