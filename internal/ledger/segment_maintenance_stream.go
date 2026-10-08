package ledger

import (
	"context"
	"errors"
)

// WalkSegments reads one committed descriptor at a time, oldest first. The
// callback must not reenter Log; holding Log.mu keeps publication and Close from
// replacing its source during the walk. Results are tentative until final nil.
// This serial maintenance interface permits no concurrent Append during IO.
func (l *Log) WalkSegments(ctx context.Context, consume func(Segment) error) error {
	if consume == nil {
		return errors.New("ledger segment consumer is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return errors.New("ledger is closed")
	}
	for generation := uint64(1); generation < l.generation; generation++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		segment, found, err := l.lookupSegmentLocked(generation)
		if err != nil {
			return err
		}
		if !found {
			return ErrSegmentMissing
		}
		if err := consume(segment); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return l.segmentDirectory.manifest.check()
}
