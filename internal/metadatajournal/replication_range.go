package metadatajournal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replicationrange"
)

// ReplicationRangeReader binds a fixed prefix cursor to the requested epoch.
// It trusts this process's authenticated immutable prefix, not the current
// integrity of the entire file. Reconciliation and full audit remain separate.
type ReplicationRangeReader struct {
	mu       sync.Mutex
	epoch    uint64
	reader   *replicationrange.Reader
	bound    *BoundReplicationAuthority
	ctx      context.Context
	path     string
	key      []byte
	closed   bool
	closeErr error
}

func NewBoundReplicationRangeReader(path string, key []byte, maxBytes int64, a BoundReplicationAuthority) (*ReplicationRangeReader, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	r := &ReplicationRangeReader{bound: &a, path: path}
	reader, err := replicationrange.New(path, key, maxBytes, r.scan)
	if err != nil {
		clear(r.key)
		return nil, err
	}
	r.reader = reader
	r.key = bytes.Clone(key)
	return r, nil
}

func NewReplicationRangeReader(path string, key []byte, maxBytes int64) (*ReplicationRangeReader, error) {
	r := &ReplicationRangeReader{path: path}
	reader, err := replicationrange.New(path, key, maxBytes, r.scan)
	if err != nil {
		clear(r.key)
		return nil, err
	}
	r.reader = reader
	r.key = bytes.Clone(key)
	return r, nil
}

func (r *ReplicationRangeReader) Read(epoch, first, last uint64) ([]byte, error) {
	return r.ReadContext(context.Background(), epoch, first, last)
}

func (r *ReplicationRangeReader) ReadContext(ctx context.Context, epoch, first, last uint64) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errors.Join(replicationrange.ErrClosed, r.closeErr)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.ctx = ctx
	defer func() { r.ctx = nil }()
	if epoch == 0 {
		return nil, errors.New("metadata replication epoch must be positive")
	}
	if r.bound != nil && epoch != r.bound.Baseline.Epoch {
		return nil, ErrIncompatibleFormat
	}
	if epoch != r.epoch {
		r.reader.Reset()
		r.epoch = epoch
	}
	frames, err := r.reader.Read(first, last)
	if err = errors.Join(err, ctx.Err()); err != nil {
		if errors.Is(err, ownedfile.ErrRetirement) {
			r.closeErr = errors.Join(r.closeErr, err)
			r.closed = true
		}
		return nil, err
	}
	return frames, nil
}

func (r *ReplicationRangeReader) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.epoch = 0
	r.reader.Reset()
}

func (r *ReplicationRangeReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.epoch = 0
	r.closed = true
	err := errors.Join(r.reader.Close(), r.closeErr)
	r.closeErr = err
	if err == nil {
		clear(r.key)
		r.key = nil
	}
	return err
}

// Full cursor/head inspection shares this same fixed reader slot with range
// delivery. Concurrent Hello/control requests cannot allocate extra scanners
// outside the NativeSource's calibrated three-reader lifetime envelope.
func (r *ReplicationRangeReader) CursorContext(ctx context.Context) (uint64, uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, 0, errors.Join(replicationrange.ErrClosed, r.closeErr)
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	var epoch, seq uint64
	var err error
	if r.bound != nil {
		epoch, seq, err = BoundReplicationCursor(ctx, r.path, r.key, *r.bound)
	} else {
		epoch, seq, err = ReplicationCursor(r.path, r.key)
	}
	if errors.Is(err, ownedfile.ErrRetirement) {
		r.closeErr = errors.Join(r.closeErr, err)
		r.closed = true
	}
	return epoch, seq, errors.Join(err, ctx.Err())
}
func (r *ReplicationRangeReader) HeadAtContext(ctx context.Context, epoch, sequence uint64) ([32]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return [32]byte{}, errors.Join(replicationrange.ErrClosed, r.closeErr)
	}
	if err := ctx.Err(); err != nil {
		return [32]byte{}, err
	}
	var digest [32]byte
	var err error
	if r.bound != nil {
		digest, err = BoundReplicationHeadAt(ctx, r.path, r.key, *r.bound, epoch, sequence)
	} else {
		digest, err = ReplicationHeadAt(r.path, r.key, epoch, sequence)
	}
	if errors.Is(err, ownedfile.ErrRetirement) {
		r.closeErr = errors.Join(r.closeErr, err)
		r.closed = true
	}
	return digest, errors.Join(err, ctx.Err())
}

func (r *ReplicationRangeReader) scan(input io.ReadSeeker, key []byte, initial replicationrange.Position, first, last uint64) (replicationrange.Range, error) {
	done := errors.New("metadata requested range authenticated")
	position := initial
	var span replicationrange.Range
	head := Head{Epoch: r.epoch, Sequence: initial.Sequence, Offset: initial.Offset, Hash: initial.Hash}
	version := uint8(frameVersion)
	if r.bound != nil {
		size, err := input.Seek(0, io.SeekEnd)
		if err != nil || initial.Offset > r.bound.MaxFileBytes || size > r.bound.MaxFileBytes-initial.Offset {
			return span, errors.Join(ErrCorrupt, err)
		}
		version = boundFrameVersion
		head.FormatVersion, head.FileRevision, head.Baseline = boundFrameVersion, r.bound.FileRevision, r.bound.Baseline
	}
	ctx := r.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	_, partial, err := scanFromContext(ctx, input, key, head, initial.Offset == 0, func(record Record) error {
		if record.FormatVersion != version {
			return errors.New("bound metadata replication requires a qualified format2 reader")
		}
		if r.bound != nil {
			if err := r.bound.checkControl(record); err != nil {
				return err
			}
		}
		if record.Epoch != r.epoch {
			return errors.New("metadata replication range belongs to a different epoch")
		}
		relativeEnd, err := input.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		end := initial.Offset + relativeEnd
		switch record.Kind {
		case KindEpoch:
			position = replicationrange.Position{Offset: end, Hash: record.Hash}
		case KindTrim:
			if first <= record.Trim.TrimmedThrough {
				return errors.New("metadata replication range precedes the authenticated trim cut")
			}
			// The successor chains to the original cut head, not to this
			// replacement anchor's own frame hash. Native scanFrom does the
			// same transition after returning from this visitor.
			position = replicationrange.Position{Sequence: record.Trim.TrimmedThrough, Offset: end}
			copy(position.Hash[:], record.Trim.ChainHeadAtTrim)
		case KindOperations:
			if record.Sequence == first {
				span.Before = position
			}
			position = replicationrange.Position{Sequence: record.Sequence, Offset: end, Hash: record.Hash}
			if record.Sequence == last {
				span.After = position
				return done
			}
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
	return replicationrange.Range{}, errors.New("metadata replication range is outside the authenticated prefix")
}
