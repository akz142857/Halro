package metadatajournal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replicationlimits"
)

func (l *Log) AppendReplicated(epoch, firstSequence, lastSequence uint64, frames []byte) error {
	return l.AppendReplicatedContext(context.Background(), epoch, firstSequence, lastSequence, frames)
}

func (l *Log) AppendReplicatedContext(ctx context.Context, epoch, firstSequence, lastSequence uint64, frames []byte) (resultErr error) {
	return l.withBoundPhysicalReceiveWorkspace(ctx, func(ctx context.Context) (resultErr error) {
		ctx, finish, err := l.beginBoundPhysicalReceive(ctx, epoch, firstSequence, lastSequence, frames)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, finish(resultErr)) }()
		return l.appendReplicatedContext(ctx, epoch, firstSequence, lastSequence, frames, NewBoundReplicationRangeReader)
	})
}

func (l *Log) appendReplicatedContext(ctx context.Context, epoch, firstSequence, lastSequence uint64, frames []byte, newBoundReader func(string, []byte, int64, BoundReplicationAuthority) (*ReplicationRangeReader, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.readOnly {
		return errors.New("read-only metadata journal refuses append")
	}
	if l.boundPhysicalRecovery {
		return ErrIncompatibleFormat
	}
	if l.freezeCancel != nil {
		return ErrNativeFrozen
	}
	if !l.replica {
		return errors.New("replicated metadata append requires replica mode")
	}
	if err := l.availabilityError(); err != nil {
		return err
	}
	if l.terminalErr != nil {
		return fmt.Errorf("metadata journal requires restart after uncertain persistence: %w", l.terminalErr)
	}
	if l.formatVersion == boundFrameVersion {
		if l.boundReplication == nil || !l.boundReplication.matches(Head{Epoch: l.epoch, Offset: l.offset, FormatVersion: l.formatVersion, FileRevision: l.fileRevision, Baseline: l.baseline}) {
			return ErrIncompatibleFormat
		}
		if len(frames) > replicationlimits.MaxPayloadBytes {
			return errors.New("bound metadata replicated batch exceeds protocol limit")
		}
		if err := l.checkPreparedFile(); err != nil {
			return err
		}
	}
	if firstSequence <= l.sequence {
		if epoch != l.epoch || lastSequence > l.sequence {
			return errors.New("replicated metadata batch partially overlaps the native cursor")
		}
		var existing []byte
		var err error
		if l.formatVersion == boundFrameVersion {
			reader, openErr := newBoundReader(l.path, l.key, int64(len(frames)), *l.boundReplication)
			if openErr != nil {
				return openErr
			}
			existing, err = reader.ReadContext(ctx, epoch, firstSequence, lastSequence)
			err = errors.Join(err, reader.Close())
			if errors.Is(err, ownedfile.ErrRetirement) {
				// Keep the actual reader and keys reachable. An unknown Close
				// cannot admit another retry or let Log.Close refund its owner.
				l.boundReaderDebt = reader
				l.retirementErr = errors.Join(l.retirementErr, err)
			}
		} else {
			existing, err = ReadReplicationFrames(l.path, l.key, epoch, firstSequence, lastSequence)
		}
		if err != nil {
			return err
		}
		if !bytes.Equal(existing, frames) {
			return errors.New("replicated metadata batch conflicts with durable native frames")
		}
		return nil
	}
	if epoch != l.epoch || firstSequence != l.sequence+1 || lastSequence < firstSequence || len(frames) == 0 {
		return errors.New("replicated metadata batch does not continue the native cursor")
	}
	if l.maxBytes > 0 && (l.offset > l.maxBytes || int64(len(frames)) > l.maxBytes-l.offset) {
		return errors.New("metadata journal disk limit reached by replicated batch")
	}
	initial := Head{Epoch: l.epoch, Sequence: l.sequence, Offset: l.offset, Hash: l.lastHash, TrimmedThrough: l.trimmedThrough, FormatVersion: l.formatVersion, FileRevision: l.fileRevision, Baseline: l.baseline}
	head, partial, err := scanFromContext(ctx, bytes.NewReader(frames), l.key, initial, false, nil)
	if err != nil || partial || head.Sequence != lastSequence || head.Offset != l.offset+int64(len(frames)) {
		if err == nil {
			err = errors.New("replicated metadata batch has an invalid boundary")
		}
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	physical, err := l.validatePhysicalPreparedLocked(ctx, int64(len(frames)))
	if err != nil {
		return err
	}
	if _, err := l.file.Seek(l.offset, io.SeekStart); err != nil {
		return err
	}
	if physical != nil {
		physical.accepted.Store(true)
	}
	if err := writeFull(l.durability, frames); err != nil {
		l.terminalErr = err
		return err
	}
	if err := l.durability.Sync(); err != nil {
		l.terminalErr = err
		return err
	}
	l.sequence, l.offset, l.lastHash = head.Sequence, head.Offset, head.Hash
	return nil
}
