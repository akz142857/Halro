package bedrock

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
)

const maxEventStreamMessage = 1 << 20

type streamMessage struct {
	Headers map[string]string
	Payload []byte
}

func readStreamMessage(reader io.Reader) (streamMessage, error) {
	var prelude [12]byte
	if _, err := io.ReadFull(reader, prelude[:]); err != nil {
		return streamMessage{}, err
	}
	total := binary.BigEndian.Uint32(prelude[0:4])
	headerLength := binary.BigEndian.Uint32(prelude[4:8])
	if total < 16 || total > maxEventStreamMessage || headerLength > total-16 {
		return streamMessage{}, errors.New("AWS event stream frame length is invalid")
	}
	if crc32.ChecksumIEEE(prelude[:8]) != binary.BigEndian.Uint32(prelude[8:12]) {
		return streamMessage{}, errors.New("AWS event stream prelude checksum mismatch")
	}
	rest := make([]byte, int(total)-12)
	if _, err := io.ReadFull(reader, rest); err != nil {
		return streamMessage{}, err
	}
	frame := append(append([]byte(nil), prelude[:]...), rest[:len(rest)-4]...)
	if crc32.ChecksumIEEE(frame) != binary.BigEndian.Uint32(rest[len(rest)-4:]) {
		return streamMessage{}, errors.New("AWS event stream message checksum mismatch")
	}
	headers, err := decodeStreamHeaders(rest[:headerLength])
	if err != nil {
		return streamMessage{}, err
	}
	payloadEnd := len(rest) - 4
	return streamMessage{Headers: headers, Payload: append([]byte(nil), rest[headerLength:payloadEnd]...)}, nil
}

func decodeStreamHeaders(encoded []byte) (map[string]string, error) {
	result := make(map[string]string)
	for offset := 0; offset < len(encoded); {
		nameLength := int(encoded[offset])
		offset++
		if nameLength == 0 || offset+nameLength+1 > len(encoded) {
			return nil, errors.New("AWS event stream header is invalid")
		}
		name := string(encoded[offset : offset+nameLength])
		offset += nameLength
		typeCode := encoded[offset]
		offset++
		switch typeCode {
		case 0:
			result[name] = "true"
		case 1:
			result[name] = "false"
		case 7:
			if offset+2 > len(encoded) {
				return nil, errors.New("AWS event stream string header is truncated")
			}
			length := int(binary.BigEndian.Uint16(encoded[offset : offset+2]))
			offset += 2
			if offset+length > len(encoded) {
				return nil, errors.New("AWS event stream string header is truncated")
			}
			result[name] = string(encoded[offset : offset+length])
			offset += length
		default:
			return nil, errors.New("AWS event stream header type is unsupported")
		}
	}
	return result, nil
}
