package ledger

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// AppendReplicated authenticates a native frame batch against the current
// Ledger chain before writing any byte, then fsyncs it.
func (l *Log) AppendReplicated(generation, firstSequence, lastSequence uint64, frames []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.options.Replica {
		return errors.New("replicated ledger append requires replica mode")
	}
	if l.file == nil {
		return errors.New("ledger is closed")
	}
	if l.status.Load() != AccountingHealthy {
		return errors.New("accounting is not healthy")
	}
	if firstSequence <= l.sequence {
		if generation != l.generation || lastSequence > l.sequence {
			return errors.New("replicated ledger batch partially overlaps the native cursor")
		}
		existing, err := ReadReplicationFrames(l.path, l.chainKey, generation, firstSequence, lastSequence)
		if err != nil {
			return err
		}
		if !bytes.Equal(existing, frames) {
			return errors.New("replicated ledger batch conflicts with durable native frames")
		}
		return nil
	}
	if generation != l.generation || firstSequence != l.sequence+1 || lastSequence < firstSequence || len(frames) == 0 {
		return errors.New("replicated ledger batch does not continue the native cursor")
	}
	verifier := &chainVerifier{
		key: append([]byte(nil), l.chainKey...), hash: l.chainHash,
		sequence: l.chainSequence, offset: l.chainOffset,
		sawFrames: l.chainSawFrames, epoch: l.chainEpoch,
	}
	defer clear(verifier.key)
	head, partial, err := scanFrom(bytes.NewReader(frames), generation, 0, l.offset, l.sequence, nil, verifier)
	if err != nil || partial || head.Sequence != lastSequence || head.Offset != l.offset+int64(len(frames)) {
		if err == nil {
			err = errors.New("replicated ledger batch has an invalid boundary")
		}
		return err
	}
	if _, err := l.file.Seek(l.offset, io.SeekStart); err != nil {
		return err
	}
	if err := writeFull(l.durability, frames); err != nil {
		l.status.MarkUnavailable()
		return err
	}
	if err := l.durability.Sync(); err != nil {
		l.status.MarkUnavailable()
		return err
	}
	l.plainDigest.Write(frames)
	l.sequence, l.offset = head.Sequence, head.Offset
	l.chainHash, l.chainSequence, l.chainOffset = verifier.hash, verifier.sequence, verifier.offset
	l.chainSawFrames, l.chainEpoch = verifier.sawFrames, verifier.epoch
	if l.sequence != lastSequence {
		return fmt.Errorf("replicated ledger ended at %d, want %d", l.sequence, lastSequence)
	}
	return nil
}
