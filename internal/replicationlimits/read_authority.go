package replicationlimits

// These shared RAM costs let a derived reducer pre-admit the actual Member and
// Ordering readers without importing the replication protocol/service package.
const (
	MaxMemberStateJSON               = 64 << 10
	MemberAuthorityRAMBytes    int64 = 4*MaxMemberStateJSON + 8192
	OrderingFrozenReadRAMBytes int64 = 64 << 10
)
