package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
)

// AppendReplicated authenticates a contiguous native batch against the local
// chain before writing any byte, then fsyncs it. It is available only on a log
// explicitly opened in Replica mode.
func (l *Log) AppendReplicated(first, last uint64, frames []byte) error {
	return l.withPhysicalEncoder(context.Background(), func(context.Context) error { return l.appendReplicated(first, last, frames) })
}
func (l *Log) appendReplicated(first, last uint64, frames []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.freezeCancel != nil {
		return ErrNativeFrozen
	}
	if !l.replica {
		return errors.New("replicated audit append requires replica mode")
	}
	if l.file == nil || l.closing {
		return errors.New("audit log is closed")
	}
	if l.terminalErr != nil {
		return fmt.Errorf("audit log requires restart after uncertain persistence: %w", l.terminalErr)
	}
	if first <= l.sequence {
		if last > l.sequence {
			return errors.New("replicated audit batch partially overlaps the native cursor")
		}
		existing, err := ReadReplicationFrames(l.path, l.key, first, last)
		if err != nil {
			return err
		}
		if !bytes.Equal(existing, frames) {
			return errors.New("replicated audit batch conflicts with durable native frames")
		}
		return nil
	}
	if first != l.sequence+1 || last < first || len(frames) == 0 {
		return errors.New("replicated audit batch does not continue the native cursor")
	}
	if l.maxDiskBytes > 0 && int64(len(frames)) > l.maxDiskBytes-l.offset {
		return ErrJournalDiskLimit
	}
	initial := Summary{Records: l.sequence, LastHash: l.lastHash, Bytes: l.offset}
	summary, partial, err := scanFrom(bytes.NewReader(frames), l.key, initial, nil)
	if err != nil || partial || summary.Records != last || summary.Bytes != l.offset+int64(len(frames)) {
		if err == nil {
			err = errors.New("replicated audit batch has an invalid boundary")
		}
		return err
	}
	if _, err := l.file.Seek(l.offset, io.SeekStart); err != nil {
		return err
	}
	physical, err := l.physicalBeforeWriteLocked(int64(len(frames)))
	if err != nil {
		return err
	}
	if err := writeFull(l.durability, frames); err != nil {
		if l.physical != nil {
			l.physical.Hold(err)
		}
		l.terminalErr = err
		return err
	}
	if err := l.durability.Sync(); err != nil {
		if l.physical != nil {
			l.physical.Hold(err)
		}
		l.terminalErr = err
		return err
	}
	if physical != nil {
		if err := physical.Durable(); err != nil {
			l.terminalErr = err
			return err
		}
	}
	l.sequence, l.offset, l.lastHash = summary.Records, summary.Bytes, summary.LastHash
	// A second fixed-row pass updates the bounded retry cache after durability.
	// No batch-sized decoded event graph remains live across native IO.
	cached, partial, err := scanFrom(bytes.NewReader(frames), l.key, initial, l.remember)
	if err != nil || partial || cached != summary {
		if err == nil {
			err = ErrCorrupt
		}
		l.terminalErr = err
		return err
	}
	return nil
}
