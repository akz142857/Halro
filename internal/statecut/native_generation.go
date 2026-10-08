package statecut

import (
	"context"
	"errors"
	"os"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
)

// NativeGeneration owns the four native readers and their selected common
// generation. Persistent reservations, publisher and directory lock remain
// application-owned, including after Close; no surviving file is refunded.
type NativeGeneration struct {
	*ReopenedStandalone
	native StandaloneOwners
}

type NativeGenerationOptions struct {
	SelectedReopenOptions
	LedgerPath, AuditPath, GovernancePath string
	AuditCachePath, GovernanceCachePath   string
	LedgerKey, AuditKey, GovernanceKey    []byte
	LedgerNative                          ledger.Options
	AuditNative                           audit.Options
	GovernanceNative                      governance.Options
}

func (g *NativeGeneration) Close() error {
	if g == nil {
		return nil
	}
	var err error
	if g.ReopenedStandalone != nil {
		err = g.ReopenedStandalone.closeWithNative(g.native.NativeLedger)
	}
	if g.native.Lease != nil {
		err = errors.Join(err, g.native.Lease.Close())
	}
	if g.native.NativeLedger != nil {
		err = errors.Join(err, selectedClosePhase("generation native Ledger", g.native.NativeLedger.Close()))
	}
	if g.native.NativeGovernance != nil {
		err = errors.Join(err, selectedClosePhase("generation native Governance", g.native.NativeGovernance.Close()))
	}
	if g.native.Audit != nil {
		err = errors.Join(err, selectedClosePhase("generation native Audit", g.native.Audit.Close()))
	}
	return err
}

// OpenSelectedGeneration selects before opening any native or root. It creates
// every selected source with the application's actual guard and Manager, and
// returns only the common generation whose real historical audits reached the
// required startup gate. StartSelectedServices completes recovery/readiness.
func OpenSelectedGeneration(ctx context.Context, o NativeGenerationOptions) (result *NativeGeneration, resultErr error) {
	s := o.Sources
	if s.NativeLedger != nil || s.Lease != nil || s.Audit != nil || s.NativeGovernance != nil || s.Publisher == nil || s.Guard == nil || s.Budget == nil || len(o.LedgerKey) != 32 || len(o.AuditKey) != 32 || len(o.GovernanceKey) != 32 || o.AuditNative.CacheBudget != s.Budget || o.GovernanceNative.CacheBudget != s.Budget {
		return nil, authenticatedstate.ErrSealIneligible
	}
	if err := s.Publisher.ValidateOwner(s.Guard, s.Budget); err != nil {
		return nil, err
	}
	if _, found, err := s.Publisher.Select(o.Authority); err != nil || !found {
		return nil, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	g := &NativeGeneration{native: s}
	defer func() {
		if resultErr != nil {
			cleanupErr := g.Close()
			resultErr = errors.Join(resultErr, cleanupErr)
			if cleanupErr != nil {
				result = g
			} else {
				result = nil
			}
		}
	}()
	acFile, err := os.Open(o.AuditCachePath)
	if err != nil {
		return nil, err
	}
	acInfo, err := acFile.Stat()
	if err != nil {
		return nil, errors.Join(err, ownedfile.CloseFile(acFile))
	}
	ac, err := audit.ReadStartupCache(ctx, acFile, acInfo.Size(), o.AuditKey, o.Authority.Instance, s.Budget)
	err = errors.Join(err, ownedfile.CloseFile(acFile))
	if ac != nil {
		defer func() { resultErr = errors.Join(resultErr, ac.Close()) }()
	}
	if err != nil {
		return nil, err
	}
	gcFile, err := os.Open(o.GovernanceCachePath)
	if err != nil {
		return nil, err
	}
	gcInfo, err := gcFile.Stat()
	if err != nil {
		return nil, errors.Join(err, ownedfile.CloseFile(gcFile))
	}
	gc, err := governance.ReadStartupCache(ctx, gcFile, gcInfo.Size(), o.GovernanceKey, o.Authority.Instance, s.Budget)
	err = errors.Join(err, ownedfile.CloseFile(gcFile))
	if gc != nil {
		defer func() { resultErr = errors.Join(resultErr, gc.Close()) }()
	}
	if err != nil {
		return nil, err
	}
	options := o.LedgerNative
	options.ChainKey = o.LedgerKey
	g.native.NativeLedger, _, err = ledger.OpenWithSelectedSeal(ctx, o.LedgerPath, ledger.NewStatus(), options, ledger.SelectedSealOpenOptions{Publisher: s.Publisher, Guard: s.Guard, Authority: o.Authority, Directory: o.DirectoryPath, DirectoryOptions: o.DirectoryOptions})
	if err != nil {
		return nil, err
	}
	g.native.Lease, err = g.native.NativeLedger.BeginMaintenance(ctx)
	if err != nil {
		return nil, err
	}
	g.native.Audit, _, err = audit.OpenWithSelectedSeal(ctx, o.AuditPath, o.AuditKey, o.AuditNative, audit.SelectedSealOpenOptions{Publisher: s.Publisher, Guard: s.Guard, Authority: o.Authority, Cache: ac, Identity: o.Authority.Instance})
	if err != nil {
		return nil, err
	}
	g.native.NativeGovernance, _, err = governance.OpenWithSelectedSeal(ctx, o.GovernancePath, o.GovernanceKey, o.GovernanceNative, governance.SelectedSealOpenOptions{Publisher: s.Publisher, Guard: s.Guard, Authority: o.Authority, Cache: gc, Identity: o.Authority.Instance})
	if err != nil {
		return nil, err
	}
	o.Sources = g.native
	g.ReopenedStandalone, err = ReopenSelectedStandalone(ctx, o.SelectedReopenOptions)
	if err != nil {
		return nil, err
	}
	return g, nil
}
