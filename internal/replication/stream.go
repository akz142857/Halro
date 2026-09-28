package replication

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

type StreamDirection uint8

const (
	StreamPrimaryToReplica StreamDirection = iota + 1
	StreamReplicaToPrimary
)

type StreamRecordKind uint8

const (
	StreamRecordFrame StreamRecordKind = iota + 1
	StreamRecordAcknowledgement
	StreamRecordCommitNotice
	StreamRecordPromotionProposal
	StreamRecordPromotionPromise
	StreamRecordHeartbeat
)

type StreamRecord struct {
	Kind    StreamRecordKind
	Encoded []byte
}

const streamPrefixBytes = 4 + 8

// ReadStreamRecord reads one post-handshake record while applying the bound
// for its actual magic before allocating the peer-declared body. Frames and
// commit notices share Primary-to-Replica connections even though their size
// limits differ by orders of magnitude; using the frame limit for everything
// would let a valid member force a 16 MiB allocation for a tiny control record.
func ReadStreamRecord(reader io.Reader, direction StreamDirection) (StreamRecord, error) {
	if reader == nil {
		return StreamRecord{}, errors.New("replication stream reader is required")
	}
	var prefix [4]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return StreamRecord{}, fmt.Errorf("read replication record length: %w", err)
	}
	declared := binary.BigEndian.Uint32(prefix[:])
	if declared < 8 {
		return StreamRecord{}, errors.New("replication record is too short to contain a type magic")
	}
	var magic [8]byte
	if _, err := io.ReadFull(reader, magic[:]); err != nil {
		return StreamRecord{}, fmt.Errorf("read replication record type: %w", err)
	}
	kind, minimum, maximum, err := streamRecordShape(magic, direction)
	if err != nil {
		return StreamRecord{}, err
	}
	if declared < minimum || declared > maximum {
		return StreamRecord{}, fmt.Errorf("replication %s record length %d is outside %d-%d", streamRecordKindName(kind), declared, minimum, maximum)
	}
	encoded := make([]byte, 4+int(declared))
	copy(encoded[:4], prefix[:])
	copy(encoded[4:streamPrefixBytes], magic[:])
	if _, err := io.ReadFull(reader, encoded[streamPrefixBytes:]); err != nil {
		return StreamRecord{}, fmt.Errorf("read replication %s record body: %w", streamRecordKindName(kind), err)
	}
	return StreamRecord{Kind: kind, Encoded: encoded}, nil
}

// WriteStreamRecord applies the same type and direction contract to local
// output. The record codecs remain responsible for semantic validation; this
// layer prevents a session from interleaving a valid record in the wrong
// direction or bypassing its type-specific size bound.
func WriteStreamRecord(writer io.Writer, direction StreamDirection, encoded []byte) error {
	if writer == nil {
		return errors.New("replication stream writer is required")
	}
	if len(encoded) < streamPrefixBytes {
		return errors.New("replication stream record is truncated")
	}
	declared := binary.BigEndian.Uint32(encoded[:4])
	if int(declared) != len(encoded)-4 {
		return errors.New("replication stream record length is not canonical")
	}
	var magic [8]byte
	copy(magic[:], encoded[4:streamPrefixBytes])
	_, minimum, maximum, err := streamRecordShape(magic, direction)
	if err != nil {
		return err
	}
	if declared < minimum || declared > maximum {
		return errors.New("replication stream record exceeds its type bound")
	}
	return writeAll(writer, encoded)
}

func streamRecordShape(magic [8]byte, direction StreamDirection) (StreamRecordKind, uint32, uint32, error) {
	var kind StreamRecordKind
	var minimum, maximum uint32
	switch {
	case bytes.Equal(magic[:], frameMagic[:]):
		kind = StreamRecordFrame
		minimum = frameBodyFixedBytes
		maximum = frameBodyFixedBytes + 2*MaxIdentityBytes + MaxMetadataBytes + MaxPayloadBytes
	case bytes.Equal(magic[:], ackMagic[:]):
		kind = StreamRecordAcknowledgement
		minimum = ackFixedBytes
		maximum = ackFixedBytes + 3*MaxIdentityBytes
	case bytes.Equal(magic[:], commitNoticeMagic[:]):
		kind = StreamRecordCommitNotice
		minimum = commitNoticeFixedBytes
		maximum = commitNoticeFixedBytes + 3*MaxIdentityBytes
	case bytes.Equal(magic[:], proposalMagic[:]):
		kind = StreamRecordPromotionProposal
		minimum = 8 + 2
		maximum = MaxPromotionRecordBytes
	case bytes.Equal(magic[:], promiseMagic[:]):
		kind = StreamRecordPromotionPromise
		minimum = 8 + 2
		maximum = MaxPromotionRecordBytes
	case bytes.Equal(magic[:], heartbeatMagic[:]):
		kind = StreamRecordHeartbeat
		minimum = heartbeatBodyBytes
		maximum = heartbeatBodyBytes
	default:
		return 0, 0, 0, errors.New("replication stream record has unknown type magic")
	}
	control := kind == StreamRecordPromotionProposal || kind == StreamRecordPromotionPromise
	allowed := control || kind == StreamRecordHeartbeat || direction == StreamPrimaryToReplica && (kind == StreamRecordFrame || kind == StreamRecordCommitNotice) ||
		direction == StreamReplicaToPrimary && kind == StreamRecordAcknowledgement
	if !allowed {
		return 0, 0, 0, fmt.Errorf("replication %s record is not allowed in this stream direction", streamRecordKindName(kind))
	}
	return kind, minimum, maximum, nil
}

func streamRecordKindName(kind StreamRecordKind) string {
	switch kind {
	case StreamRecordFrame:
		return "frame"
	case StreamRecordAcknowledgement:
		return "acknowledgement"
	case StreamRecordCommitNotice:
		return "commit-notice"
	case StreamRecordPromotionProposal:
		return "promotion-proposal"
	case StreamRecordPromotionPromise:
		return "promotion-promise"
	case StreamRecordHeartbeat:
		return "heartbeat"
	default:
		return "unknown"
	}
}
