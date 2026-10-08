// Package governancelimits shares lower bounds without coupling configuration
// decoding to the Governance implementation.
package governancelimits

const (
	MaxCachedEventIDs                 = 4096
	StartupCacheReadRAMBytes    int64 = 512 << 10
	StartupCachePersistRAMBytes int64 = 128 << 10
	// Fixed fingerprints/positions, bounded hash-table growth, ring, keys and
	// owner metadata. A logical lower bound; allocator amplification requires
	// calibration. Replay/batch/codec temporaries require separate envelopes.
	MinCacheRAMBytes int64 = 2 << 20
)
