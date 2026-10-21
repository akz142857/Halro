package governance

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

func (l *Log) AppendReplicated(first, last uint64, frames []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.replica {
		return errors.New("replicated governance append requires replica mode")
	}
	if l.file == nil {
		return errors.New("governance journal is closed")
	}
	if l.terminalErr != nil {
		return fmt.Errorf("governance journal requires restart after uncertain persistence: %w", l.terminalErr)
	}
	if first <= l.sequence {
		if last > l.sequence {
			return errors.New("replicated governance batch partially overlaps the native cursor")
		}
		existing, err := ReadReplicationFrames(l.path, l.key, first, last)
		if err != nil {
			return err
		}
		if !bytes.Equal(existing, frames) {
			return errors.New("replicated governance batch conflicts with durable native frames")
		}
		return nil
	}
	if first != l.sequence+1 || last < first || len(frames) == 0 {
		return errors.New("replicated governance batch does not continue the native cursor")
	}
	initial := Summary{Records: l.sequence, LastHash: l.lastHash, Bytes: l.offset}
	var records []Record
	summary, partial, err := scanFrom(bytes.NewReader(frames), l.key, initial, func(record Record) error {
		records = append(records, record)
		return nil
	})
	if err != nil || partial || summary.Records != last || summary.Bytes != l.offset+int64(len(frames)) {
		if err == nil {
			err = errors.New("replicated governance batch has an invalid boundary")
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
	l.sequence, l.offset, l.lastHash = summary.Records, summary.Bytes, summary.LastHash
	for _, record := range records {
		l.remember(record)
	}
	return nil
}
