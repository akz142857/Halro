package replication

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// HeartbeatProtocolVersion is an optional post-handshake record capability.
// Existing data records remain protocol version 1; a peer advertising only
// version 1 must never receive a heartbeat record.
const HeartbeatProtocolVersion uint16 = 2

const heartbeatBodyBytes = 8 + 2 + 2

var heartbeatMagic = [8]byte{'H', 'L', 'R', 'H', 'B', '0', '0', '2'}

func encodeHeartbeat() []byte {
	encoded := make([]byte, 4+heartbeatBodyBytes)
	binary.BigEndian.PutUint32(encoded[:4], heartbeatBodyBytes)
	copy(encoded[4:12], heartbeatMagic[:])
	binary.BigEndian.PutUint16(encoded[12:14], HeartbeatProtocolVersion)
	return encoded
}

func validateHeartbeat(encoded []byte) error {
	if len(encoded) != 4+heartbeatBodyBytes || binary.BigEndian.Uint32(encoded[:4]) != heartbeatBodyBytes ||
		!bytes.Equal(encoded[4:12], heartbeatMagic[:]) ||
		binary.BigEndian.Uint16(encoded[12:14]) != HeartbeatProtocolVersion ||
		binary.BigEndian.Uint16(encoded[14:16]) != 0 {
		return errors.New("replication heartbeat record is invalid")
	}
	return nil
}
