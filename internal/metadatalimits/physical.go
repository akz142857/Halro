package metadatalimits

import (
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const (
	DeltaRAMBytes       int64 = 64 << 20
	TransactionRAMBytes int64 = 32 << 20
)

// PhysicalCompletionProfile is the actual original Metadata window's active
// and terminal workspace cost. ha describes a profile, never source authority.
// Every call returns a fresh list; callers cannot mutate another owner's pool.
func PhysicalCompletionProfile(ha bool) (resourcebudget.Cost, []resourcebudget.WorkspaceRequirement) {
	const auxiliaryIO = int64(256 << 10)
	const snapshotRAM = int64(16 << 10)
	r := []resourcebudget.WorkspaceRequirement{
		{Class: resourcebudget.Background, RAM: authenticatedstate.UpdateRAMBytes},
		{Class: resourcebudget.Background, RAM: snapshotRAM},
		{Class: resourcebudget.IO, RAM: auxiliaryIO},
		{Class: resourcebudget.IO, RAM: authenticatedstate.ReadRAMBytes},
	}
	if ha {
		r = append(r,
			resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: replicationlimits.MemberAuthorityRAMBytes},
			resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: replicationlimits.OrderingFrozenReadRAMBytes})
	}
	return resourcebudget.Cost{RAM: DeltaRAMBytes + TransactionRAMBytes + 4096}, r
}
