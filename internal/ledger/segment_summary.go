package ledger

import (
	"fmt"
	"math"
)

// SegmentSummary describes committed manifest entries, not physical allocation,
// orphan compression files, cache/RSS or permission to remove native history.
type SegmentSummary struct {
	Generations uint64
	PlainBytes  int64
	StoredBytes int64
}

func appendSegmentSummary(summary SegmentSummary, segment Segment) (SegmentSummary, error) {
	if summary.Generations == math.MaxUint64 || summary.PlainBytes < 0 || summary.StoredBytes < 0 || segment.Length < 0 || segment.StoredLength < 0 || segment.Length > math.MaxInt64-summary.PlainBytes || segment.StoredLength > math.MaxInt64-summary.StoredBytes {
		return SegmentSummary{}, fmt.Errorf("%w: sealed segment byte totals are invalid", ErrCorrupt)
	}
	summary.Generations++
	summary.PlainBytes += segment.Length
	summary.StoredBytes += segment.StoredLength
	return summary, nil
}

// SealedSummary returns only fixed-size counters maintained at the same locked
// commit boundary as the native manifest. It neither copies nor walks history.
func (l *Log) SealedSummary() SegmentSummary {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.segmentSummary
}

// GenerationForSequence returns the sealed or active generation whose
// sequence envelope covers sequence. It uses a lower-bound locator query and
// decodes at most one sealed descriptor; it does not assert that every integer
// inside a legacy gap represents an actual frame.
func (l *Log) GenerationForSequence(sequence uint64) (uint64, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return 0, false, fmt.Errorf("ledger is closed")
	}
	if sequence == 0 {
		return 0, false, nil
	}
	if sequence > l.startSequence && sequence <= l.sequence {
		return l.generation, true, nil
	}
	generation, found, err := l.segmentDirectory.FindSequenceCandidate(sequence)
	if err != nil {
		l.status.RequireRecovery()
		return 0, false, err
	}
	if !found {
		return 0, false, nil
	}
	segment, found, err := l.lookupSegmentLocked(generation)
	if err != nil {
		return 0, false, err
	}
	if !found || sequence < segment.FirstSequence || sequence > segment.LastSequence {
		return 0, false, nil
	}
	return generation, true, nil
}

// NextCompactableSegment selects at most one committed descriptor. Selection
// does not grant retention/confirmation authority: through is supplied by the
// caller's existing archive/checkpoint policy and Compact rechecks the entry.
// The authenticated ordinal locator is scanned as fixed-size slots. Only the
// selected descriptor is decoded from the manifest.
func (l *Log) NextCompactableSegment(through uint64) (Segment, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return Segment{}, false, fmt.Errorf("ledger is closed")
	}
	generation, found, err := l.segmentDirectory.FindFirstUncompressedThrough(through)
	if err != nil {
		l.status.RequireRecovery()
		return Segment{}, false, err
	}
	if !found {
		return Segment{}, false, nil
	}
	segment, found, err := l.lookupSegmentLocked(generation)
	if err != nil {
		return Segment{}, false, err
	}
	if !found || segment.Compressed || segment.LastSequence > through {
		l.status.RequireRecovery()
		return Segment{}, false, fmt.Errorf("%w: compactable directory hint changed", ErrCorrupt)
	}
	return segment, true, nil
}
