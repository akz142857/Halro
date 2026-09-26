package governance

import (
	"errors"
	"fmt"
	"io"
	"os"
)

func ReplicationCursor(path string, key []byte) (uint64, uint64, error) {
	summary, err := Verify(path, key)
	if err != nil {
		return 0, 0, err
	}
	return 1, summary.Records, nil
}

func ReplicationHeadAt(path string, key []byte, generation, sequence uint64) ([32]byte, error) {
	if generation != 1 {
		return [32]byte{}, errors.New("governance replication generation must be 1")
	}
	if sequence == 0 {
		return [32]byte{}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return [32]byte{}, err
	}
	defer file.Close()
	var head [32]byte
	summary, partial, err := scan(file, key, func(record Record) error {
		if record.Sequence == sequence {
			head = record.Hash
		}
		return nil
	})
	if err != nil {
		return [32]byte{}, err
	}
	if partial {
		return [32]byte{}, fmt.Errorf("%w: partial final record", ErrCorrupt)
	}
	if sequence > summary.Records || head == ([32]byte{}) {
		return [32]byte{}, errors.New("governance replication baseline is outside the durable journal")
	}
	return head, nil
}

func ReadReplicationFrames(path string, key []byte, first, last uint64) ([]byte, error) {
	if first == 0 || last < first {
		return nil, errors.New("governance replication range is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var start, end int64 = -1, -1
	var previous int64
	_, partial, err := scan(file, key, func(record Record) error {
		if record.Sequence == first {
			start = previous
		}
		if record.Sequence == last {
			end = record.Offset
		}
		previous = record.Offset
		return nil
	})
	if err != nil {
		return nil, err
	}
	if partial {
		return nil, fmt.Errorf("%w: partial final record", ErrCorrupt)
	}
	if start < 0 || end < start {
		return nil, errors.New("governance replication range is outside the durable journal")
	}
	frames := make([]byte, end-start)
	if _, err := file.ReadAt(frames, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return frames, nil
}

func TruncateReplicationTail(path string, key []byte, sequence uint64) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	offset := int64(0)
	summary, partial, err := scan(file, key, func(record Record) error {
		if record.Sequence == sequence {
			offset = record.Offset
		}
		return nil
	})
	if err != nil {
		return err
	}
	if partial {
		return fmt.Errorf("%w: partial final record", ErrCorrupt)
	}
	if sequence > summary.Records || sequence > 0 && offset == 0 {
		return errors.New("governance truncation cursor is outside the durable journal")
	}
	if err := file.Truncate(offset); err != nil {
		return err
	}
	return file.Sync()
}
