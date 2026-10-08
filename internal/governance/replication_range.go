package governance

import (
	"errors"
	"fmt"
	"io"

	"github.com/akz142857/Halro/internal/replicationrange"
)

// NewReplicationRangeReader reads ordered ranges with one authenticated prefix
// cursor. Unlike ReadReplicationFrames it does not audit bytes after the range;
// startup reconciliation must authenticate the physical source first.
func NewReplicationRangeReader(path string, key []byte, maxBytes int64) (*replicationrange.Reader, error) {
	return replicationrange.New(path, key, maxBytes, scanReplicationRange)
}

func scanReplicationRange(file io.ReadSeeker, key []byte, initial replicationrange.Position, first, last uint64) (replicationrange.Range, error) {
	done := errors.New("governance requested range authenticated")
	position := initial
	var span replicationrange.Range
	_, partial, err := scanFrom(file, key, Summary{Records: initial.Sequence, Bytes: initial.Offset, LastHash: initial.Hash}, func(record Record) error {
		if record.Sequence == first {
			span.Before = position
		}
		position = replicationrange.Position{Sequence: record.Sequence, Offset: record.Offset, Hash: record.Hash}
		if record.Sequence == last {
			span.After = position
			return done
		}
		return nil
	})
	if errors.Is(err, done) {
		return span, nil
	}
	if err != nil {
		return replicationrange.Range{}, err
	}
	if partial {
		return replicationrange.Range{}, fmt.Errorf("%w: partial requested replication frame", ErrCorrupt)
	}
	return replicationrange.Range{}, errors.New("governance replication range is outside the authenticated prefix")
}
