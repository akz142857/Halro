// Package replicationlimits defines protocol bounds without runtime dependencies.
package replicationlimits

const (
	DefaultQueueCapacity                  = 64
	DefaultMaxQueuedBytes                 = 64 << 20
	NativeReaderSets                      = 3                              // one default set and at most two peer leases
	MinNativeReaderRAMBytes         int64 = NativeReaderSets * (128 << 10) // only a calibration lower bound
	MaxBudgetedPeerSessions               = 3                              // two peers and one replacement/control session
	MaxPrimaryUnconfirmedEntries          = 65536
	PrimaryUnconfirmedEntryRAMBytes int64 = 64 // includes terminal-credit class ownership
	PrimaryUnconfirmedTableRAMBytes int64 = MaxPrimaryUnconfirmedEntries * PrimaryUnconfirmedEntryRAMBytes
	QueueRecordRAMBytes             int64 = 256 // lower estimate, including optional done channel
	QueueFixedRAMBytes              int64 = 1024
	MinQueueRAMBytes                int64 = DefaultMaxQueuedBytes + (DefaultQueueCapacity+1)*QueueRecordRAMBytes + QueueFixedRAMBytes
	MaxIdentityBytes                      = 64
	MaxMetadataBytes                      = 4 << 10
	MaxPayloadBytes                       = 16 << 20
	FrameBodyFixedBytes                   = 108
	MaxEncodedFrameBytes                  = 4 + FrameBodyFixedBytes + 2*MaxIdentityBytes + MaxMetadataBytes + MaxPayloadBytes
	// This lower bound rejects obviously undersized envelopes. Native decoding
	// and JSON amplification still require measured deployment calibration.
	MinCandidateRAMBytes int64 = 6 * MaxEncodedFrameBytes
	// Wire body, decoded copies and synchronous handler temporary amplification.
	// Retained projections and TLS allocations require separate owners.
	MinReadRAMBytes int64 = 6 * MaxEncodedFrameBytes
)
