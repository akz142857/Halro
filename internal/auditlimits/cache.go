// Package auditlimits shares structural lower bounds without coupling config
// decoding to the native Audit implementation.
package auditlimits

const (
	MaxCachedEventIDs                 = 4096
	StartupCacheReadRAMBytes    int64 = 512 << 10
	StartupCachePersistRAMBytes int64 = 128 << 10
	// Fixed fingerprints, positions, map, ring and owner metadata. This logical
	// floor requires allocator calibration; codec/batch temporaries need their
	// own working envelopes.
	MinCacheRAMBytes int64 = 2 << 20
)
