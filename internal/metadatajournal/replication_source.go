package metadatajournal

import (
	"errors"
	"fmt"
	"io"
	"os"
)

func ReplicationCursor(path string, key []byte) (uint64, uint64, error) {
	head, err := Verify(path, key)
	if err != nil {
		return 0, 0, err
	}
	return head.Epoch, head.Sequence, nil
}

func ReplicationHeadAt(path string, key []byte, epoch, sequence uint64) ([32]byte, error) {
	if epoch == 0 {
		return [32]byte{}, errors.New("metadata replication epoch must be positive")
	}
	var chainHead [32]byte
	head, err := Replay(path, key, func(record Record) error {
		if record.Epoch != epoch {
			return nil
		}
		switch {
		case sequence == 0 && record.Kind == KindEpoch:
			chainHead = record.Hash
		case record.Kind == KindOperations && record.Sequence == sequence:
			chainHead = record.Hash
		case record.Kind == KindTrim && record.Trim.TrimmedThrough == sequence:
			copy(chainHead[:], record.Trim.ChainHeadAtTrim)
		}
		return nil
	})
	if err != nil {
		return [32]byte{}, err
	}
	if head.Epoch != epoch || sequence < head.TrimmedThrough || sequence > head.Sequence || chainHead == ([32]byte{}) {
		return [32]byte{}, errors.New("metadata replication baseline is outside the durable journal")
	}
	return chainHead, nil
}

func ReadReplicationFrames(path string, key []byte, epoch, first, last uint64) ([]byte, error) {
	if epoch == 0 || first == 0 || last < first {
		return nil, errors.New("metadata replication range is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var start, end int64 = -1, -1
	head, err := Replay(path, key, func(record Record) error {
		if record.Epoch != epoch || record.Kind != KindOperations {
			return nil
		}
		if record.Sequence == first {
			start = record.Offset
		}
		if record.Sequence == last {
			end = record.Offset + frameSize(record, file)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if head.Epoch != epoch || start < 0 || end < start {
		return nil, errors.New("metadata replication range is outside the durable journal")
	}
	frames := make([]byte, end-start)
	if _, err := file.ReadAt(frames, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return frames, nil
}

func TruncateReplicationTail(path string, key []byte, epoch, sequence uint64) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	offset := int64(-1)
	head, partial, err := scan(file, key, func(record Record) error {
		if record.Epoch != epoch {
			return nil
		}
		if record.Kind == KindEpoch && sequence == 0 || record.Kind == KindOperations && record.Sequence == sequence {
			offset = record.Offset + frameSize(record, file)
		}
		if record.Kind == KindTrim && record.Trim.TrimmedThrough == sequence {
			offset = record.Offset + frameSize(record, file)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if partial {
		return fmt.Errorf("%w: partial final frame", ErrCorrupt)
	}
	if head.Epoch != epoch || sequence < head.TrimmedThrough || sequence > head.Sequence || offset < 0 {
		return errors.New("metadata truncation cursor is outside the durable journal")
	}
	if err := file.Truncate(offset); err != nil {
		return err
	}
	return file.Sync()
}
