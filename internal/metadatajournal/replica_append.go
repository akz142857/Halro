package metadatajournal

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

func (l *Log) AppendReplicated(epoch, firstSequence, lastSequence uint64, frames []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.replica {
		return errors.New("replicated metadata append requires replica mode")
	}
	if l.file == nil {
		return errors.New("metadata journal is closed")
	}
	if l.terminalErr != nil {
		return fmt.Errorf("metadata journal requires restart after uncertain persistence: %w", l.terminalErr)
	}
	if firstSequence <= l.sequence {
		if epoch != l.epoch || lastSequence > l.sequence {
			return errors.New("replicated metadata batch partially overlaps the native cursor")
		}
		existing, err := ReadReplicationFrames(l.path, l.key, epoch, firstSequence, lastSequence)
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
	initial := Head{Epoch: l.epoch, Sequence: l.sequence, Offset: l.offset, Hash: l.lastHash, TrimmedThrough: l.trimmedThrough}
	head, partial, err := scanFrom(bytes.NewReader(frames), l.key, initial, false, nil)
	if err != nil || partial || head.Sequence != lastSequence || head.Offset != l.offset+int64(len(frames)) {
		if err == nil {
			err = errors.New("replicated metadata batch has an invalid boundary")
		}
		return err
	}
	if _, err := l.file.Seek(l.offset, io.SeekStart); err != nil {
		return err
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
