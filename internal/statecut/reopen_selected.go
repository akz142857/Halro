package statecut

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/startupaudit"
	"github.com/akz142857/Halro/internal/usage"
)

type SelectedReopenOptions struct {
	ReopenOptions
	MetadataBinding  metadatajournal.BaselineBinding
	AuditOptions     startupaudit.Options
	PhysicalRecovery *ledger.PhysicalStartupRecoveryOptions
}

// ReopenSelectedStandalone initializes all logical roots from one actual
// selected envelope and private selected-native construction provenance. Only
// native suffixes are reduced. Four bounded historical readers must each make
// authenticated progress before business admission; structural publication
// remains gated until all actual readers finish. Caller retains native logs,
// publisher, process guard and their persistent fees. No HA role is inferred.
func ReopenSelectedStandalone(ctx context.Context, o SelectedReopenOptions) (result *ReopenedStandalone, resultErr error) {
	s, a := o.Sources, o.Authority
	if err := startupaudit.Validate(o.AuditOptions); err != nil {
		return nil, err
	}
	if s.Budget == nil || s.Publisher == nil || s.Guard == nil || s.NativeLedger == nil || s.Lease == nil || s.NativeGovernance == nil || s.Audit == nil || s.InstanceID == "" || s.KeyGeneration == 0 || s.Ledger != nil || s.Usage != nil || s.Governance != nil || s.Metadata != nil || s.Directory != nil || a.Role != authenticatedstate.SealStandalone || a.Coverage != authenticatedstate.SealCoverageAuthenticated || a.Instance == ([32]byte{}) || o.LedgerOptions.Identity != a.Instance || o.LedgerOptions.Budget != s.Budget || o.DirectoryOptions.Identity != a.Instance || o.DirectoryOptions.Budget != s.Budget || o.UsageOptions.Identity != a.Instance || o.UsageOptions.Budget != s.Budget || o.GovernanceOptions.Identity != a.Instance || o.GovernanceOptions.Budget != s.Budget || o.MetadataOptions.Identity != a.Instance || o.MetadataOptions.Budget != s.Budget || o.MetadataOptions.InstanceID != s.InstanceID || o.MetadataOptions.KeyGeneration != s.KeyGeneration {
		return nil, authenticatedstate.ErrSealIneligible
	}
	if err := s.Publisher.ValidateOwner(s.Guard, s.Budget); err != nil {
		return nil, err
	}
	if replicated, err := s.Lease.ValidateOwner(s.NativeLedger); err != nil || replicated {
		return nil, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	selected, found, err := s.Publisher.Select(a)
	if err != nil || !found {
		return nil, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	hard := HardLimits()
	if selected.RootCount != uint8(len(hard.RequiredRoots)) {
		return nil, authenticatedstate.ErrSealIneligible
	}
	for i, schema := range hard.RequiredRoots {
		r := selected.Roots[i]
		if (authenticatedstate.SealRootSchema{Kind: r.Kind, Component: r.Component, IndexID: r.IndexID, Codec: r.Codec}) != schema {
			return nil, authenticatedstate.ErrSealIneligible
		}
	}
	lp := selected.Positions[0]
	if lp.Generation == 0 || lp.Offset > math.MaxInt64 || lp.Epoch > math.MaxUint8 || lp.Revision != 0 {
		return nil, authenticatedstate.ErrSealIneligible
	}
	if err := s.Audit.ValidateSelectedStartupOwner(ctx, s.Budget, s.Publisher, s.Guard, a, a.Instance); err != nil {
		return nil, err
	}
	work, err := s.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 128 << 10})
	if err != nil {
		return nil, err
	}
	r := &ReopenedStandalone{Owners: s, Seal: selected, selectedStartup: true}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, r.Close())
			result = nil
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) || errors.Is(resultErr, ledger.ErrSegmentRetirement) {
			r.cleanupErr, r.debt, result = resultErr, work, r
		} else if err := work.Release(); err != nil {
			resultErr = errors.Join(resultErr, err, r.Close())
			r.cleanupErr, r.debt, result = resultErr, work, r
		}
	}()
	if o.PhysicalRecovery != nil {
		r.startupAudits[0], err = s.NativeLedger.StartSelectedStartupAudit(ctx, s.Budget, o.AuditOptions)
		if err == nil {
			err = r.startupAudits[0].Wait(ctx)
		}
		if err != nil {
			return nil, fmt.Errorf("full native physical recovery authority: %w", err)
		}
	}
	so := ledger.SelectedLedgerProjectionOpenOptions{Publisher: s.Publisher, Guard: s.Guard, Authority: a, PhysicalRecovery: o.PhysicalRecovery}
	var saved, head ledger.ReplayAnchor
	if err := s.NativeLedger.WithSelectedStartupOwner(ctx, s.Budget, so, a.Instance, func(ctx context.Context, source ledger.SelectedStartupSource) error {
		if source.SelectedSeal() != selected {
			return authenticatedstate.ErrSealIneligible
		}
		saved, head = source.SavedAnchor(), source.NativeHead()
		return source.Check(ctx)
	}); err != nil {
		return nil, err
	}
	lo := ledger.AuthenticatedLedgerOpenOptions{AuthenticatedLedgerOptions: o.LedgerOptions, SavedAnchor: saved, DiskReservation: o.LedgerDisk}
	lo.Prefix = &ledger.ProjectionPrefix{Head: head.Head, Hash: head.Hash}
	r.Owners.Ledger, r.LedgerReceipt, err = ledger.OpenSelectedAuthenticatedLedgerProjection(ctx, s.NativeLedger, o.LedgerDirectory, lo, so)
	if err != nil {
		return nil, fmt.Errorf("open selected Ledger root: %w", err)
	}
	if r.Owners.Ledger.PhysicalStartupRecoveryPending() {
		writer := selectedRecoveryWriter{owners: r.Owners}
		r.physicalStartupRecovery, err = budget.RecoverAuthenticatedRequests(ctx, r.Owners.Ledger, &writer, budget.RecoveryStandalone, ledger.ProjectionPageLimits{Rows: 64, Bytes: 8 << 20})
		if err != nil {
			return nil, fmt.Errorf("consume selected physical recovery debt: %w", err)
		}
		if err := r.Owners.Ledger.FinishPhysicalStartupRecovery(ctx, s.NativeLedger); err != nil {
			return nil, err
		}
		r.physicalRecovered = true
	}
	var lr ledger.AuthenticatedLedgerSealReference
	if err := s.NativeLedger.WithSelectedStartupOwner(ctx, s.Budget, so, a.Instance, func(ctx context.Context, source ledger.SelectedStartupSource) error {
		var err error
		lr, err = r.Owners.Ledger.SealReference(ctx)
		if err == nil {
			lr.Root, lr.Anchor = selected.Roots[0], saved
		}
		return err
	}); err != nil {
		return nil, err
	}
	uo := o.UsageOptions
	uo.Native, uo.Owner = s.NativeLedger, r.Owners.Ledger
	ur := usage.TypedUsageReference{Ledger: lr, Projection: usage.AuthenticatedUsageSealReference{Schema: hard.RequiredRoots[5], Root: selected.Roots[5], Scope: selected.Roots[5].State.Scope, Position: selected.Positions[4]}}
	r.Derivative, r.Owners.Usage, err = usage.OpenSelectedTypedDiskLedgerDerivative(ctx, uo, s.Lease, ur, o.UsageDisk, so)
	if err != nil {
		return nil, fmt.Errorf("open selected Usage root: %w", err)
	}
	goOptions := o.GovernanceOptions
	goOptions.Native = s.NativeGovernance
	gr := governance.TypedGovernanceReference{Journal: governance.GovernanceSealReference{Schema: hard.RequiredRoots[2], Root: selected.Roots[2], Position: selected.Positions[2]}, Projection: governance.AuthenticatedGovernanceProjectionReference{Schema: hard.RequiredRoots[3], Root: selected.Roots[3], Scope: selected.Roots[3].State.Scope, Position: selected.Positions[2]}}
	r.Owners.Governance, _, err = governance.OpenSelectedTypedGovernanceProjection(ctx, goOptions, governance.SelectedTypedGovernanceOpenOptions{Publisher: s.Publisher, Guard: s.Guard, Authority: a, Selected: gr, DiskReservation: o.GovernanceDisk, Permitted: s.NativeGovernance.Summary()})
	if err != nil {
		return nil, fmt.Errorf("open selected Governance root: %w", err)
	}
	mo := o.MetadataOptions
	r.Owners.Metadata, r.MetadataReceipt, err = metadatastate.OpenSelectedFromRoot(ctx, o.MetadataKey, o.MetadataDirectory, o.MetadataJournalPath, metadatastate.SelectedOpenOptions{BaselineOptions: mo.BaselineOptions, Evidence: mo.Evidence, Binding: o.MetadataBinding, Publisher: s.Publisher, Guard: s.Guard, Authority: a, RootDiskReservation: mo.RootDiskReservation, JournalDiskReservation: mo.JournalDiskReservation, MaxJournalBytes: mo.MaxJournalBytes, PhysicalTerminalReserve: o.PhysicalRecovery != nil})
	if err != nil {
		return nil, fmt.Errorf("open selected Metadata root: %w", err)
	}
	dr := ledger.AuthenticatedDirectoryReference{Schema: hard.RequiredRoots[6], Root: selected.Roots[6], Position: lp}
	r.Owners.Directory, err = ledger.OpenSelectedAuthenticatedDirectory(ctx, s.NativeLedger, dr, o.DirectoryPath, o.DirectoryOptions, so)
	if err != nil {
		return nil, fmt.Errorf("open selected directory root: %w", err)
	}
	auditOptions := o.AuditOptions
	onFailure := auditOptions.OnFailure
	auditOptions.OnFailure = func(err error) {
		// A failed Metadata/Audit/Governance grant withdraws this same native
		// Ledger's global accounting/admission status as well. It cannot leave
		// a partially authenticated common generation serving business.
		s.NativeLedger.Status().RequireRecovery()
		if onFailure != nil {
			onFailure(err)
		}
	}
	if r.startupAudits[0] == nil {
		r.startupAudits[0], err = s.NativeLedger.StartSelectedStartupAudit(ctx, s.Budget, auditOptions)
	}
	if err == nil {
		r.startupAudits[1], err = s.NativeGovernance.StartSelectedStartupAudit(ctx, s.Budget, auditOptions)
	}
	if err == nil {
		r.startupAudits[2], err = s.Audit.StartSelectedStartupAudit(ctx, s.Budget, auditOptions)
	}
	if err == nil {
		r.startupAudits[3], err = r.Owners.Metadata.StartSelectedStartupAudit(ctx, auditOptions)
	}
	if err != nil {
		return nil, fmt.Errorf("start selected historical audit: %w", err)
	}
	for i, audit := range r.startupAudits {
		if err := audit.WaitReady(ctx); err != nil {
			return nil, fmt.Errorf("selected historical audit %d readiness: %w", i, err)
		}
	}
	current, found, err := s.Publisher.Select(a)
	if err != nil || !found || current != selected {
		return nil, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	result = r
	return result, ctx.Err()
}

// WaitStartupAudit joins all four actual historical readers. Finite deadlines
// and no-progress errors still withdraw native/DAO read and write eligibility.
func (r *ReopenedStandalone) WaitStartupAudit(ctx context.Context) error {
	if r == nil {
		return authenticatedstate.ErrSealIneligible
	}
	r.mu.Lock()
	audits, closed := r.startupAudits, r.closed
	r.mu.Unlock()
	if closed || !r.selectedStartup {
		return authenticatedstate.ErrSealIneligible
	}
	for _, audit := range audits {
		if err := audit.Wait(ctx); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// BusinessHealthError rechecks the original four finite grants, including
// immediate deadline expiry before the monitor next runs. It lends no source
// and does not turn Ready into structural/full-history completion.
func (r *ReopenedStandalone) BusinessHealthError(ctx context.Context) error {
	if r == nil || !r.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	audits, closed, selected := r.startupAudits, r.closed, r.selectedStartup
	r.mu.Unlock()
	if closed || !selected {
		return authenticatedstate.ErrClosed
	}
	for _, audit := range audits {
		if audit == nil {
			return authenticatedstate.ErrSealIneligible
		}
		p := audit.Snapshot()
		if !p.Ready || p.Err != nil {
			return errors.Join(authenticatedstate.ErrRecovery, p.Err)
		}
	}
	return ctx.Err()
}
