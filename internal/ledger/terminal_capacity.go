package ledger

import "github.com/akz142857/Halro/internal/replicationlimits"

// TerminalReplicationFrameBytes bounds one isolated native Ledger event and
// its exact HA wire envelope, including the maximum cluster identities. The
// writer never combines unrelated work with a terminal credit owner.
const TerminalReplicationFrameBytes int64 = maxPayloadSize + 256 + replicationlimits.FrameBodyFixedBytes + 4 + 2*replicationlimits.MaxIdentityBytes
