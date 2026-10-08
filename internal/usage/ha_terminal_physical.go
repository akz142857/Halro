package usage

import (
	"context"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func qualifiedPhysicalUsageRole(role authenticatedstate.SealRole, ha bool) bool {
	if ha {
		return role == authenticatedstate.SealPrimary || role == authenticatedstate.SealReplica
	}
	return role == authenticatedstate.SealStandalone
}

// HAPhysicalCatchupWorkspaceRequirements describes the actual paid catch-up
// pool. It grants no source, role, recovery or publication permission.
func HAPhysicalCatchupWorkspaceRequirements() []resourcebudget.WorkspaceRequirement {
	r := typedLiveWorkspaceRequirements()
	r[3].RAM = replicationlimits.MemberAuthorityRAMBytes
	r[4].RAM = replicationlimits.OrderingFrozenReadRAMBytes
	return r
}

const PhysicalCatchupActiveRAMBytes int64 = usageSealWorkspaceRAMBytes

// TypedProjectionResidentRAMBytes belongs to the live projection, separately
// from its physical catch-up work. Closing a catch-up job does not release it.
const TypedProjectionResidentRAMBytes int64 = usageSealWorkspaceRAMBytes

// Only the original selected HA source and its active full-cut maintenance
// authority may capture a physical owner; Standalone entry points keep their
// role gate. The native registration independently authenticates the same lease.
func (p *DiskProjection) EnableHAPhysicalCatchup(ctx context.Context, native *ledger.Log, owner *ledger.AuthenticatedLedgerProjection, lease *ledger.MaintenanceLease, selection [32]byte) error {
	return p.enablePhysicalCatchup(ctx, native, owner, lease, selection, true)
}

func (p *DiskProjection) RetireHAPhysicalCatchupAfterCommon(ctx context.Context, native *ledger.Log, owner *ledger.AuthenticatedLedgerProjection, lease *ledger.MaintenanceLease, publisher *authenticatedstate.SealCandidatePublisher) error {
	return p.retirePhysicalCatchupAfterCommon(ctx, native, owner, lease, publisher, true)
}

func (p *DiskProjection) BindHAPhysicalCommonSelection(ctx context.Context, native *ledger.Log, owner *ledger.AuthenticatedLedgerProjection, selection [32]byte) error {
	return p.bindPhysicalCommonSelection(ctx, native, owner, selection, true)
}
