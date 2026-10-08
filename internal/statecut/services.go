package statecut

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/usage"
)

type SelectedServicesOptions struct {
	Authority              authenticatedstate.SealEligibility
	Timezone               string
	QueueCapacity          int
	Accounting             budget.Options
	PhysicalRecoveryCaches func(context.Context) (StartupCacheCandidates, error)
}

// SelectedServices borrows one actual selected generation. Close of the common
// owner retires these managers and joins the Collector before closing any root.
// The caller must still drain complete requests, and owns the native logs,
// Publisher, persistent fees and process lock. This does not grant HA authority.
type SelectedServices struct {
	Accounting *budget.Manager
	Governance *governance.Manager
	Metadata   *boltstore.Store
	Collector  *usage.Collector
	Recovery   budget.ProjectedRecoveryResult
}

func (s *SelectedServices) retire(ctx context.Context) error {
	if s.Accounting != nil {
		s.Accounting.WithdrawAdmission()
	}
	// Governance may call the accounting reader; join it before releasing that
	// reader, and join accounting observers before retiring their target.
	if s.Governance != nil {
		if err := s.Governance.Retire(ctx); err != nil {
			return err
		}
	}
	if s.Accounting != nil {
		if err := s.Accounting.Retire(ctx); err != nil {
			return err
		}
	}
	if s.Collector != nil {
		return s.Collector.Retire(ctx)
	}
	return nil
}

// StartSelectedServices performs the actual recovery-to-business handoff, rather
// than exposing a restored root as a ready service. Finite historical grants,
// original two-level recovery inputs, Usage catch-up and DAO price pins are all
// checked under the same startup lease. No manager escapes before that lease is
// released. Failure closes the generation; repeated startup cannot reopen it.
// Historical readers continue under their original deadlines/progress gates;
// structural publication remains denied until they all physically complete.
func (r *ReopenedStandalone) StartSelectedServices(ctx context.Context, o SelectedServicesOptions) (result *SelectedServices, resultErr error) {
	if r == nil {
		return nil, authenticatedstate.ErrSealIneligible
	}
	r.mu.Lock()
	claimed := false
	defer func() {
		r.mu.Unlock()
		if claimed && resultErr != nil {
			resultErr = errors.Join(resultErr, r.Close())
			result = nil
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.closed || !r.selectedStartup || r.cleanupErr != nil || r.services != nil || r.metadataStore != nil || o.QueueCapacity < 1 || o.QueueCapacity > 65536 || o.Timezone == "" {
		return nil, authenticatedstate.ErrSealIneligible
	}
	selected, found, err := r.Owners.Publisher.Select(o.Authority)
	if err != nil || !found || selected != r.Seal {
		return nil, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	// Validate timezone before accepting ownership of the startup operation.
	if _, err := budget.NewFixedPeriodResolver(o.Timezone); err != nil {
		return nil, err
	}
	s := &SelectedServices{}
	r.services, claimed = s, true
	r.servicesMu.Lock()
	if r.servicesClosing {
		r.servicesMu.Unlock()
		return nil, authenticatedstate.ErrSealIneligible
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
	for _, audit := range r.startupAudits {
		if r.physicalRecovered {
			if err := audit.Wait(ctx); err != nil {
				return nil, err
			}
		}
		if err := audit.WaitReady(ctx); err != nil {
			return nil, err
		}
	}
	writer := selectedRecoveryWriter{owners: r.Owners}
	s.Recovery, err = budget.RecoverAuthenticatedRequests(ctx, r.Owners.Ledger, &writer, budget.RecoveryStandalone, ledger.ProjectionPageLimits{Rows: 64, Bytes: 8 << 20})
	if err != nil {
		return nil, fmt.Errorf("recover selected accounting: %w", err)
	}
	if r.physicalRecovered {
		s.Recovery.AttemptsSettled += r.physicalStartupRecovery.AttemptsSettled
		s.Recovery.RequestsFinalized += r.physicalStartupRecovery.RequestsFinalized
		s.Recovery.InputHead = r.physicalStartupRecovery.InputHead
	}
	if _, err := r.Derivative.CatchUpAuthenticatedFromNative(ctx, r.Owners.NativeLedger, r.Owners.Lease, r.Owners.Budget); err != nil {
		return nil, fmt.Errorf("catch up selected Usage before services: %w", err)
	}
	s.Metadata, err = r.openMetadataStoreLocked(ctx, o.Authority)
	if err != nil {
		return nil, fmt.Errorf("open selected Metadata facade: %w", err)
	}
	now := time.Now()
	if o.Accounting.Now != nil {
		now = o.Accounting.Now()
	}
	settings, err := s.Metadata.SeedInstanceAccountingSettings(o.Timezone, now)
	if err != nil {
		return nil, fmt.Errorf("seed selected accounting settings: %w", err)
	}
	periods, err := budget.NewPeriodResolver(settings)
	if err != nil {
		return nil, err
	}
	s.Accounting, err = budget.NewWithSelectedAuthenticatedProjection(ctx, r.Owners.NativeLedger, r.Owners.Ledger, periods, r.Owners.Lease, r.Owners.Budget, o.Accounting)
	if err != nil {
		return nil, fmt.Errorf("create selected accounting manager: %w", err)
	}
	if err := s.Metadata.RecoverDeploymentPricePins(ctx, r.Owners.Ledger); err != nil {
		return nil, fmt.Errorf("recover selected price pins: %w", err)
	}
	s.Governance, err = governance.NewTypedProjectedManager(ctx, r.Owners.NativeGovernance, r.Owners.Governance, s.Accounting, s.Metadata, r.Owners.Budget)
	if err != nil {
		return nil, err
	}
	if r.physicalRecovered {
		if o.PhysicalRecoveryCaches == nil {
			return nil, authenticatedstate.ErrSealIneligible
		}
		if _, err := r.Owners.Directory.AdvanceFromNative(ctx, r.Owners.NativeLedger, r.Owners.Lease, r.Owners.Ledger); err != nil {
			return nil, err
		}
		if err := r.Owners.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
			_, err := r.publishCurrentSelectedRootsLocked(ctx, o.PhysicalRecoveryCaches)
			return err
		}); err != nil {
			return nil, fmt.Errorf("publish recovered physical common cut: %w", err)
		}
		// Startup still owns the original actual maintenance lease. The whole
		// publication above, rather than any individual component's NoDebt,
		// permits vector retirement and rebinding before Ready.
		previousLease := r.checkpointLease
		r.checkpointLease = r.Owners.Lease
		finishErr := r.finishPhysicalCommonSelectionLocked(context.WithoutCancel(ctx))
		r.checkpointLease = previousLease
		if finishErr != nil {
			return nil, fmt.Errorf("finish recovered physical common cut: %w", finishErr)
		}
	}
	if err := r.Owners.Usage.ActivateTypedLive(ctx, r.Owners.NativeLedger, r.Owners.Budget); err != nil {
		return nil, fmt.Errorf("activate selected live Usage: %w", err)
	}
	s.Collector, err = usage.NewTypedLiveCollector(r.Derivative, r.Owners.NativeLedger, r.Owners.Budget, o.QueueCapacity)
	if err != nil {
		return nil, err
	}
	if err := s.Accounting.AddObserver(s.Collector.Observe); err != nil {
		return nil, err
	}
	if err := r.Owners.Ledger.EnableSelectedBusinessReads(ctx, r.Owners.NativeLedger, r.Owners.Lease, r.Owners.Budget); err != nil {
		return nil, err
	}
	if err := r.Owners.Lease.Close(); err != nil {
		return nil, err
	}
	return s, ctx.Err()
}

// Only StartSelectedServices constructs this writer, before exposing business
// handles. Standalone confirmation binds the actual durable/applied head; it
// does not substitute for HA confirmation or accept a caller's bare watermark.
type selectedRecoveryWriter struct{ owners StandaloneOwners }

func (w *selectedRecoveryWriter) ConfirmRecoveryPrefix(ctx context.Context, expected ledger.Watermark) error {
	return w.owners.Lease.WithHead(ctx, func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		if replicated, err := w.owners.Lease.ValidateOwner(w.owners.NativeLedger); err != nil || replicated {
			return errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		actual, actualHash, err := w.owners.Ledger.ReadPosition(ctx)
		if err != nil || head != expected || actual != head || actualHash != hash {
			return errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		return w.owners.NativeLedger.ValidateSelectedStartupRead(ctx)
	})
}

func (w *selectedRecoveryWriter) AppendApplyRecovery(ctx context.Context, event ledger.Event) (ledger.Record, error) {
	if err := w.owners.Ledger.ValidateSelectedNativeWriter(ctx, w.owners.NativeLedger, w.owners.Lease, w.owners.Budget); err != nil {
		return ledger.Record{}, err
	}
	var record ledger.Record
	var receipt ledger.AppendReceipt
	err := w.owners.Ledger.WithPhysicalStartupRecovery(ctx, event.Kind, func(ctx context.Context) error {
		return w.owners.Ledger.WithNativeApplyWorkspace(ctx, func(scopedCtx context.Context) error {
			var err error
			record, receipt, err = w.owners.Lease.AppendApply(scopedCtx, event, func(applyCtx context.Context, record ledger.Record) error {
				_, err := w.owners.Ledger.AdvanceOneFromNative(applyCtx, w.owners.NativeLedger, ledger.ProjectionPrefix{Head: ledger.Watermark{Generation: record.Generation, Sequence: record.Sequence, Offset: record.Offset}, Hash: record.Hash})
				return err
			})
			return err
		})
	})
	if err != nil {
		return ledger.Record{}, err
	}
	if receipt.ReplicationIndex != 0 || receipt.Watermark != (ledger.Watermark{Generation: record.Generation, Sequence: record.Sequence, Offset: record.Offset}) {
		return ledger.Record{}, authenticatedstate.ErrSealIneligible
	}
	return record, nil
}
