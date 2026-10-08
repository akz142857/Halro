package statecut

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// CheckpointSelectedServices withdraws the business generation and publishes
// all seven actual roots after its producers have joined. The application must
// drain complete HTTP/provider requests before calling it. It cannot resume the
// retired managers: a new ready generation must be selected and reopened.
// Supplied cache files must have been captured after that application drain.
// A failed handoff closes the generation without discarding persistent facts.
func (r *ReopenedStandalone) CheckpointSelectedServices(ctx context.Context, caches StartupCacheCandidates) (Publication, error) {
	return r.checkpointSelectedServices(ctx, false, resourcebudget.Permit{}, func(context.Context) (StartupCacheCandidates, error) { return caches, nil })
}

// CheckpointLiveSelectedServices pauses the same business owners, publishes a
// complete common cut and resumes them. The caller must first join whole HTTP
// and deferred provider work. prepare captures and persists the inactive cache
// slot after all DAO/accounting/collector producers have joined. A rejected
// candidate keeps the previously selected seal and the durable native suffix.
func (r *ReopenedStandalone) CheckpointLiveSelectedServices(ctx context.Context, prepare func(context.Context) (StartupCacheCandidates, error)) (Publication, error) {
	return r.checkpointSelectedServices(ctx, true, resourcebudget.Permit{}, prepare)
}

// CheckpointLiveSelectedServicesWithRepack additionally reclaims obsolete COW
// pages with one fixed application-owned reservation. The first common cut
// makes old-slot deletion eligible; a second common cut selects compact packs.
func (r *ReopenedStandalone) CheckpointLiveSelectedServicesWithRepack(ctx context.Context, prepare func(context.Context) (StartupCacheCandidates, error), reservation resourcebudget.Permit) (Publication, error) {
	return r.checkpointSelectedServices(ctx, true, reservation, prepare)
}

func (r *ReopenedStandalone) checkpointSelectedServices(ctx context.Context, live bool, reservation resourcebudget.Permit, prepare func(context.Context) (StartupCacheCandidates, error)) (result Publication, resultErr error) {
	if r == nil || r.Owners.Directory == nil {
		return result, authenticatedstate.ErrSealIneligible
	}
	claimed := false
	resultErr = r.Owners.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		var err error
		result, err = r.checkpointSelectedServicesWithWorkspace(ctx, live, reservation, prepare, &claimed)
		return err
	})
	// Retirement must join after the original workspace callback has exited;
	// a failing handoff cannot close the pool it is still borrowing itself.
	if claimed && !live && resultErr != nil {
		resultErr = errors.Join(resultErr, r.Close())
	}
	return result, resultErr
}

func (r *ReopenedStandalone) checkpointSelectedServicesWithWorkspace(ctx context.Context, live bool, reservation resourcebudget.Permit, prepare func(context.Context) (StartupCacheCandidates, error), claimed *bool) (result Publication, resultErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if r.closed || !r.selectedStartup || r.services == nil || r.Seal.Revision == math.MaxUint64 || prepare == nil {
		return result, authenticatedstate.ErrSealIneligible
	}
	*claimed = true
	r.servicesMu.Lock()
	if r.servicesClosing {
		r.servicesMu.Unlock()
		return result, authenticatedstate.ErrSealIneligible
	}
	ctx, cancel := context.WithCancel(ctx)
	r.servicesCancel = cancel
	r.servicesMu.Unlock()
	defer func() {
		cancel()
		r.servicesMu.Lock()
		r.servicesCancel = nil
		r.servicesMu.Unlock()
	}()
	s := r.services
	if live {
		resumeGovernance, err := s.Governance.PauseOperations(ctx)
		if err != nil {
			return result, fmt.Errorf("pause selected Governance for checkpoint: %w", err)
		}
		defer func() { resultErr = errors.Join(resultErr, resumeGovernance()) }()
		resumeAccounting, err := s.Accounting.PauseOperations(ctx)
		if err != nil {
			return result, fmt.Errorf("pause selected Accounting for checkpoint: %w", err)
		}
		defer func() { resultErr = errors.Join(resultErr, resumeAccounting()) }()
		resultErr = s.Collector.WithPausedCatchUp(ctx, func(ctx context.Context) (callbackErr error) {
			resumeMetadata, err := s.Metadata.PauseFacade(ctx)
			if err != nil {
				return fmt.Errorf("pause selected Metadata for checkpoint: %w", err)
			}
			defer func() { callbackErr = errors.Join(callbackErr, resumeMetadata()) }()
			result, err = r.publishSelectedCheckpointLocked(ctx, true, reservation, prepare)
			return err
		})
		return result, resultErr
	}
	s.Accounting.WithdrawAdmission()
	if err := s.Governance.Retire(ctx); err != nil {
		return result, err
	}
	if err := s.Accounting.Retire(ctx); err != nil {
		return result, err
	}
	if err := s.Collector.CatchUp(ctx); err != nil {
		return result, err
	}
	if err := s.Collector.Retire(ctx); err != nil {
		return result, err
	}
	if err := s.Metadata.RetireFacade(ctx); err != nil {
		return result, err
	}
	return r.publishSelectedCheckpointLocked(ctx, false, resourcebudget.Permit{}, prepare)
}

func (r *ReopenedStandalone) publishSelectedCheckpointLocked(ctx context.Context, live bool, reservation resourcebudget.Permit, prepare func(context.Context) (StartupCacheCandidates, error)) (result Publication, resultErr error) {
	lease, err := r.Owners.NativeLedger.BeginMaintenance(ctx)
	if err != nil {
		return result, err
	}
	previousLease := r.Owners.Lease
	r.Owners.Lease = lease
	r.checkpointLease = lease
	bound := false
	if live {
		defer func() {
			// Preserve source identity even if the caller canceled after durable Publish.
			// Healthy candidate rejection can resume; a latched native/root fault cannot.
			var restoreErr error
			if bound {
				restoreErr = r.Owners.Usage.ActivateTypedLive(context.WithoutCancel(ctx), r.Owners.NativeLedger, r.Owners.Budget)
			}
			resultErr = errors.Join(resultErr, restoreErr, lease.Close())
			r.Owners.Lease, r.checkpointLease = previousLease, nil
		}()
	}
	if err := alignStandaloneMaintenanceRoll(ctx, r.Owners, lease); err != nil {
		return result, fmt.Errorf("align selected Ledger maintenance Roll: %w", err)
	}
	if err := r.Owners.Usage.BindTypedMaintenance(ctx, r.Owners.NativeLedger, lease, r.Owners.Ledger, r.Owners.Budget); err != nil {
		return result, fmt.Errorf("bind selected Usage maintenance: %w", err)
	}
	bound = true
	if _, err := r.Owners.Directory.AdvanceFromNative(ctx, r.Owners.NativeLedger, lease, r.Owners.Ledger); err != nil {
		return result, fmt.Errorf("advance selected Ledger directory: %w", err)
	}
	result, resultErr = r.publishCurrentSelectedRootsLocked(ctx, prepare)
	if resultErr == nil {
		resultErr = r.finishPhysicalCommonSelectionLocked(context.WithoutCancel(ctx))
	}
	if resultErr != nil || !live {
		return result, resultErr
	}
	if r.Seal.Revision == math.MaxUint64 {
		return result, nil
	}
	if _, err := reservation.Cost(); err != nil {
		return result, nil
	}
	return r.repackSelectedRootsLocked(ctx, prepare, reservation, result)
}

// Services and the actual native maintenance lease have joined all producers.
// This grants only a completed local physical Roll with identical logical
// facts, not a semantic catch-up, a caller-supplied cut or HA role permission.
func alignStandaloneMaintenanceRoll(ctx context.Context, o StandaloneOwners, lease *ledger.MaintenanceLease) error {
	if replicated, err := lease.ValidateOwner(o.NativeLedger); err != nil || replicated {
		return errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	return lease.ReadFrozenHead(ctx, func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		from, err := o.Ledger.ReadReplayAnchor(ctx)
		if err != nil {
			return err
		}
		if from.Head == head && from.Hash == hash {
			return ctx.Err()
		}
		if head.Offset != 0 || head.Sequence != from.Head.Sequence || hash != from.Hash || head.Generation <= from.Head.Generation {
			return fmt.Errorf("%w: maintenance head is not a completed physical Roll", ledger.ErrProjectionSource)
		}
		receipt, err := o.Ledger.AdvanceFromNative(ctx, o.NativeLedger, ledger.ProjectionPrefix{Head: head, Hash: hash})
		if err != nil || receipt.AppliedRecords != 0 || receipt.To.Head != head || receipt.To.Hash != hash || receipt.To.Epoch != from.Epoch {
			return fmt.Errorf("maintenance Roll reduction failed: %w", errors.Join(ledger.ErrProjectionSource, err))
		}
		return ctx.Err()
	})
}

func (r *ReopenedStandalone) publishCurrentSelectedRootsLocked(ctx context.Context, prepare func(context.Context) (StartupCacheCandidates, error)) (result Publication, resultErr error) {
	resultErr = r.Owners.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		var err error
		result, err = r.publishCurrentSelectedRootsWithWorkspaceLocked(ctx, prepare)
		return err
	})
	return result, resultErr
}

func (r *ReopenedStandalone) publishCurrentSelectedRootsWithWorkspaceLocked(ctx context.Context, prepare func(context.Context) (StartupCacheCandidates, error)) (result Publication, resultErr error) {
	// Identity, key generation and standalone role were selected independently
	// at startup. Only the positions are advanced, from actual authenticated
	// owners; Publish checks all of them again under the common source freeze.
	authority := authenticatedstate.SealEligibility{
		Instance: r.Seal.Instance, Incarnation: r.Seal.Incarnation,
		SchemaVersion: r.Seal.SchemaVersion, ApplyVersion: r.Seal.ApplyVersion,
		IndexVersion: r.Seal.IndexVersion, KeyDerivation: r.Seal.KeyDerivation,
		Role: r.Seal.Role, Coverage: r.Seal.Coverage,
	}
	base := r.Seal
	base.Revision++
	result, resultErr = PublishStandaloneCapturedStartupCaches(ctx, r.Owners, base, authority, prepare)
	if resultErr != nil {
		resultErr = fmt.Errorf("publish all selected roots with native startup caches: %w", resultErr)
	}
	if resultErr == nil {
		r.Seal = result.Seal

	}
	return result, resultErr
}

// The seven actual roots have already passed common publication. Retire only
// captured unused targets, retaining control banks through the selection change.
// Repack calls this after its actual candidates commit; component candidates
// cannot bind physical authority to a selection that has not adopted them.
func (r *ReopenedStandalone) finishPhysicalCommonSelectionLocked(ctx context.Context) error {
	return r.Owners.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		return r.finishPhysicalCommonSelectionWithWorkspaceLocked(ctx)
	})
}

func (r *ReopenedStandalone) finishPhysicalCommonSelectionWithWorkspaceLocked(ctx context.Context) (resultErr error) {
	defer func() {
		if resultErr != nil {
			r.Owners.NativeLedger.Status().RequireRecovery()
		}
	}()
	o := r.Owners
	if err := o.Directory.RetirePhysicalCatchupAfterCommon(ctx, o.NativeLedger, o.Ledger, r.checkpointLease, o.Publisher); err != nil {
		return fmt.Errorf("retire actual common Directory preparation: %w", err)
	}
	if err := o.Usage.RetirePhysicalCatchupAfterCommon(ctx, o.NativeLedger, o.Ledger, r.checkpointLease, o.Publisher); err != nil {
		return fmt.Errorf("retire actual common Usage preparation: %w", err)
	}
	if err := o.Metadata.RetirePhysicalPreparationAfterPinRecovery(ctx); err != nil {
		return fmt.Errorf("retire actual common Metadata preparation: %w", err)
	}
	if err := o.NativeLedger.RetirePhysicalPreparationForMaintenance(ctx, r.checkpointLease); err != nil {
		return fmt.Errorf("retire actual common Ledger preparation: %w", err)
	}
	encoded, err := json.Marshal(r.Seal)
	if err != nil {
		return err
	}
	selection := sha256.Sum256(encoded)
	if err := o.NativeLedger.BindPhysicalSelectionDigestContext(ctx, selection); err != nil {
		return fmt.Errorf("bind actual common Ledger selection: %w", err)
	}
	if err := o.Metadata.BindPhysicalCommonSelection(ctx); err != nil {
		return fmt.Errorf("bind actual common Metadata selection: %w", err)
	}
	if err := o.Usage.BindPhysicalCommonSelection(ctx, o.NativeLedger, o.Ledger, selection); err != nil {
		return fmt.Errorf("bind actual common Usage selection: %w", err)
	}
	return o.Directory.BindPhysicalCommonSelection(ctx, o.NativeLedger, o.Ledger, selection)
}
