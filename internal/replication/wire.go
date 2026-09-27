package replication

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ReadLengthDelimited reads exactly one canonical record without trusting the
// peer-controlled length for allocation. maxBody is specific to the record
// type expected in the current protocol state.
func ReadLengthDelimited(reader io.Reader, maxBody uint32) ([]byte, error) {
	if maxBody == 0 {
		return nil, errors.New("record size bound must be positive")
	}
	var prefix [4]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return nil, fmt.Errorf("read record length: %w", err)
	}
	length := binary.BigEndian.Uint32(prefix[:])
	if length == 0 || length > maxBody {
		return nil, fmt.Errorf("record length %d exceeds the allowed bound", length)
	}
	encoded := make([]byte, 4+int(length))
	copy(encoded[:4], prefix[:])
	if _, err := io.ReadFull(reader, encoded[4:]); err != nil {
		return nil, fmt.Errorf("read record body: %w", err)
	}
	return encoded, nil
}

func WriteLengthDelimited(writer io.Writer, encoded []byte, maxBody uint32) error {
	if len(encoded) < 4 || binary.BigEndian.Uint32(encoded[:4]) != uint32(len(encoded)-4) {
		return errors.New("record length is not canonical")
	}
	if len(encoded)-4 > int(maxBody) {
		return errors.New("record exceeds the allowed bound")
	}
	return writeAll(writer, encoded)
}
