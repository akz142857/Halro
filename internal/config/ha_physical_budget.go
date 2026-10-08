package config

import (
	"fmt"
	"math"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatalimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/usage"
)

// HAPhysicalWorkProfile describes consumer shapes, not source
// authority or a sufficient RSS/capacity promise. Metadata covers one accepted
// Primary price-pin window or the original Replica receive window. More client
// concurrency, native/control owners and temporary jobs remain additional.
type HAPhysicalWorkProfile struct {
	Name         string
	Class        resourcebudget.Class
	Active       resourcebudget.Cost
	Requirements []resourcebudget.WorkspaceRequirement
}

// HAPhysicalResidentWorkRequirements covers the selected projection lifetimes
// which overlap every physical work pool. The five original authenticated
// stores are Ledger, Usage, Governance, Metadata and Directory. Query pins,
// collector queues, native retry caches and additional owners remain extra.
func (b ResourceBudget) HAPhysicalResidentWorkRequirements() []resourcebudget.WorkspaceRequirement {
	r := []resourcebudget.WorkspaceRequirement{
		{Class: resourcebudget.Background, RAM: usage.TypedProjectionResidentRAMBytes},
		{Class: resourcebudget.IO, RAM: governance.TypedProjectionResidentRAMBytes},
	}
	for range 5 {
		r = append(r, resourcebudget.WorkspaceRequirement{Class: resourcebudget.Background, RAM: authenticatedstate.StoreResidentRAMBytes})
	}
	return r
}

func (b ResourceBudget) HAPhysicalWorkProfiles() []HAPhysicalWorkProfile {
	metadata, slots := metadatalimits.PhysicalCompletionProfile(true)
	return []HAPhysicalWorkProfile{
		{"Directory checkpoint", resourcebudget.IO, resourcebudget.Cost{RAM: 4096}, ledger.HAPhysicalCheckpointWorkspaceRequirements()},
		{"Usage catch-up", resourcebudget.IO, resourcebudget.Cost{RAM: usage.PhysicalCatchupActiveRAMBytes}, usage.HAPhysicalCatchupWorkspaceRequirements()},
		{"Governance projection", resourcebudget.IO, resourcebudget.Cost{RAM: 4096}, governance.HAPhysicalProjectionWorkspaceRequirements()},
		{"Metadata terminal/receive", resourcebudget.IO, metadata, slots},
		{"one Ledger request", resourcebudget.Active, resourcebudget.Cost{RAM: b.RequestRAMBytes}, ledger.HASelectedCompletionWorkspaceRequirements()},
	}
}

// Only the three background consumers use the foundation's shared RAM lane.
// Metadata windows and request completion retain their independent guarantees.
func (b ResourceBudget) HAPhysicalBackgroundSharedProfiles() []resourcebudget.SharedWorkProfile {
	var profiles []resourcebudget.SharedWorkProfile
	for _, p := range b.HAPhysicalWorkProfiles()[:3] {
		profiles = append(profiles, resourcebudget.SharedWorkProfile{Class: p.Class, Active: p.Active, Requirements: p.Requirements})
	}
	return profiles
}

// HAPhysicalWorkMinimum counts active, reserved terminal and control bytes
// together. A completion slot borrowed during a job remains charged, so moving
// it between IO/Background/Completion is not a reduction in simultaneous RAM.
func (b ResourceBudget) HAPhysicalWorkMinimum() (normal, completion int64, claims int, err error) {
	add := func(v int64) bool {
		if v < 0 || v > math.MaxInt64-normal {
			return false
		}
		normal += v
		return true
	}
	shared, err := resourcebudget.SharedWorkspaceRequirements(b.HAPhysicalBackgroundSharedProfiles())
	if err != nil || !add(resourcebudget.SharedWorkspaceControlRAMBytes) {
		return 0, 0, 0, resourcebudget.ErrInvalid
	}
	claims = 1 + len(shared)
	for _, r := range shared {
		if !add(r.RAM) {
			return 0, 0, 0, resourcebudget.ErrInvalid
		}
		completion += r.RAM
	}
	for i, p := range b.HAPhysicalWorkProfiles() {
		claims++
		if p.Active.RAM <= 0 || !add(p.Active.RAM) {
			return 0, 0, 0, resourcebudget.ErrInvalid
		}
		if i < 3 {
			continue // original member fees; shared terminal slots counted above
		}
		claims += len(p.Requirements)
		for _, r := range p.Requirements {
			if r.RAM <= 0 || !add(r.RAM) {
				return 0, 0, 0, resourcebudget.ErrInvalid
			}
			completion += r.RAM
		}
	}
	for _, r := range b.HAPhysicalResidentWorkRequirements() {
		if r.RAM <= 0 || !add(r.RAM) {
			return 0, 0, 0, resourcebudget.ErrInvalid
		}
		claims++
	}
	claims++ // Independent pricing IO overlaps the accepted request.
	if !add(b.PricingIORAMBytes) || !add(int64(claims)*resourcebudget.ClaimRAMBytes) {
		return 0, 0, 0, resourcebudget.ErrInvalid
	}
	return normal, completion, claims, nil
}

func (b ResourceBudget) validateHAPhysicalWorkMinimum() error {
	if !b.MemoryStateRequired || !b.PhysicalTerminalReserve {
		return nil
	}
	if b.TotalRAMBytes <= 0 || b.RecoveryReserveRAMBytes < 0 || b.RecoveryClaims < 0 {
		return fmt.Errorf("HA physical work vector: %w", resourcebudget.ErrInvalid)
	}
	normal, completion, claims, err := b.HAPhysicalWorkMinimum()
	if err != nil {
		return fmt.Errorf("HA physical work vector: %w", err)
	}
	if b.TotalRAMBytes < b.RecoveryReserveRAMBytes || normal > b.TotalRAMBytes-b.RecoveryReserveRAMBytes {
		return fmt.Errorf("HA physical work vector requires at least %d normal RAM bytes alongside Recovery reserve %d (total %d); other persistent owners and additional concurrency are extra", normal, b.RecoveryReserveRAMBytes, b.TotalRAMBytes)
	}
	if completion > b.CompletionRAMBytes || claims > b.MaxClaims-b.RecoveryClaims {
		return fmt.Errorf("HA physical work vector requires at least %d Completion RAM bytes and %d normal claims", completion, claims)
	}
	return nil
}
