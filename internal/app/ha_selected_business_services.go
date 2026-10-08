package app

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/usage"
)

// These managers borrow only this actual selected generation. Runtime must
// withdraw whole HTTP/producers first; the owner joins every service before
// retiring any Ledger/Governance/Usage/Metadata dependency.
type haSelectedBusinessServices struct {
	accounting   *budget.Manager
	governance   *governance.Manager
	collector    *usage.Collector
	collectorFee resourcebudget.Permit
}

func (s *haSelectedBusinessServices) close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if s.accounting != nil {
		s.accounting.WithdrawAdmission()
	}
	if s.governance != nil {
		if err := s.governance.Retire(ctx); err != nil {
			return err
		}
	}
	if s.accounting != nil {
		if err := s.accounting.Retire(ctx); err != nil {
			return err
		}
	}
	if s.collector != nil {
		if err := s.collector.Retire(ctx); err != nil {
			return err
		}
	}
	if s.collectorFee != (resourcebudget.Permit{}) {
		if err := s.collectorFee.Release(); err != nil {
			return err
		}
		s.collectorFee = resourcebudget.Permit{}
	}
	return nil
}

// This factory runs while the actual selected startup lease is still installed.
// Period settings come from the independently qualified Metadata facade. Missing
// settings require an actual Primary Metadata operation and confirmation first;
// configuration alone is never substituted for the persisted accounting zone.
func (f *haSelectedRuntimeFoundation) installHASelectedAccountingService(ctx context.Context) error {
	if f == nil || f.reopened == nil || f.metadataStore == nil || f.replication == nil || f.ctx.Err() != nil || f.business != nil && f.business.accounting != nil {
		return authenticatedstate.ErrSealIneligible
	}
	if f.replication.role != replication.RolePrimary {
		return authenticatedstate.ErrSealIneligible
	}
	settings, err := f.metadataStore.InstanceAccountingSettingsContext(ctx)
	if err != nil {
		return err
	}
	periods, err := budget.NewPeriodResolver(settings)
	if err != nil {
		return err
	}
	c := f.reopened.Owners.Components
	m, err := budget.NewWithHASelectedAuthenticatedProjection(ctx, c.NativeLedger, c.Ledger, periods, c.Lease, c.Budget, budget.Options{})
	if err != nil {
		return err
	}
	if f.business == nil {
		f.business = &haSelectedBusinessServices{}
	}
	f.business.accounting = m
	return nil
}

func (f *haSelectedRuntimeFoundation) installHASelectedUsageCollector(ctx context.Context) error {
	if f == nil || f.reopened == nil || f.replication == nil || f.ctx.Err() != nil || f.business != nil && f.business.collector != nil {
		return authenticatedstate.ErrSealIneligible
	}
	capacity := f.cfg.Usage.AnalyticsQueueCapacity
	if capacity < 1 || capacity > 65536 {
		return resourcebudget.ErrInvalid
	}
	c := f.reopened.Owners.Components
	fee, err := c.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{RAM: 4096 + int64(capacity)*8})
	if err != nil {
		return err
	}
	if f.business == nil {
		f.business = &haSelectedBusinessServices{}
	}
	f.business.collectorFee = fee
	collector, err := usage.NewTypedLiveCollector(f.reopened.Derivative, c.NativeLedger, c.Budget, capacity)
	if err != nil {
		return err
	}
	f.business.collector = collector
	if f.business.accounting != nil {
		return f.business.accounting.AddObserver(collector.Observe)
	}
	return nil
}

func (f *haSelectedRuntimeFoundation) installHASelectedGovernanceService(ctx context.Context) error {
	if f == nil || f.reopened == nil || f.metadataStore == nil || f.replication == nil || f.ctx.Err() != nil || f.business != nil && f.business.governance != nil {
		return authenticatedstate.ErrSealIneligible
	}
	if f.business == nil {
		f.business = &haSelectedBusinessServices{}
	}
	c := f.reopened.Owners.Components
	var reader ledger.AccountingWorkUnitReader = c.Ledger
	if f.business.accounting != nil {
		reader = f.business.accounting
	}
	var manager *governance.Manager
	var err error
	open := func(ctx context.Context) error {
		if f.replication.role == replication.RolePrimary {
			manager, err = governance.NewHASelectedTypedProjectedManager(ctx, c.NativeGovernance, c.Governance, reader, f.metadataStore, c.Budget)
		} else if f.replication.role == replication.RoleReplica {
			manager, err = governance.NewHASelectedTypedReadManager(ctx, c.NativeGovernance, c.Governance, reader, f.metadataStore, c.Budget)
		} else {
			return authenticatedstate.ErrSealIneligible
		}
		return err
	}
	if f.cfg.ResourceBudget.PhysicalTerminalReserve {
		err = c.Directory.WithPhysicalCatchupWorkspace(ctx, open)
	} else {
		err = open(ctx)
	}
	if err != nil {
		return errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	f.business.governance = manager
	return nil
}
