package replication

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	ProtocolVersion  uint16 = 1
	MaxIdentityBytes        = 64
	MaxMetadataBytes        = 4 << 10
	MaxPayloadBytes         = 16 << 20

	frameBodyFixedBytes = 108
	frameDigestOffset   = 76
)

var (
	frameMagic  = [8]byte{'H', 'L', 'R', 'P', 'L', '0', '0', '1'}
	frameDomain = []byte("halro:replication-frame:v1\x00")
)

type Kind uint16

const (
	KindData                  Kind = 1
	KindLedgerRoll            Kind = 2
	KindLeadershipEstablished Kind = 3
	KindSchemaBoundary        Kind = 4
)

type Store uint16

const (
	StoreNone       Store = 0
	StoreLedger     Store = 1
	StoreAudit      Store = 2
	StoreGovernance Store = 3
	StoreMetadata   Store = 4
)

// Frame is one version-1 physical replication record. Payload is already
// durable store bytes, not a semantic mutation. Metadata is kind-specific and
// remains bounded separately from payload so a control record cannot smuggle a
// store-sized allocation through its descriptor.
type Frame struct {
	Kind           Kind
	Store          Store
	Index          uint64
	Term           uint64
	ConfirmedIndex uint64
	// StoreGeneration is the physical file generation that owns Payload.
	// Ledger uses its active generation and metadata uses its journal epoch;
	// Audit and Governance use generation 1. Keeping it in the authenticated
	// envelope prevents an epoch reset from looking like a sequence fork.
	StoreGeneration    uint64
	StoreSequenceFirst uint64
	StoreSequenceLast  uint64
	ClusterID          string
	Incarnation        string
	Metadata           []byte
	Payload            []byte
}

func (f Frame) Validate() error {
	if f.Index == 0 || f.Term == 0 {
		return errors.New("replication frame index and term must be positive")
	}
	if f.ConfirmedIndex >= f.Index {
		return errors.New("replication frame confirmed index must precede its own index")
	}
	if len(f.ClusterID) == 0 || len(f.ClusterID) > MaxIdentityBytes ||
		len(f.Incarnation) == 0 || len(f.Incarnation) > MaxIdentityBytes {
		return fmt.Errorf("replication frame identities must be 1-%d bytes", MaxIdentityBytes)
	}
	if len(f.Metadata) > MaxMetadataBytes {
		return fmt.Errorf("replication frame metadata exceeds %d bytes", MaxMetadataBytes)
	}
	if len(f.Payload) > MaxPayloadBytes {
		return fmt.Errorf("replication frame payload exceeds %d bytes", MaxPayloadBytes)
	}

	switch f.Kind {
	case KindData:
		if f.Store < StoreLedger || f.Store > StoreMetadata {
			return errors.New("data replication frame has an invalid store")
		}
		if len(f.Payload) == 0 || len(f.Metadata) != 0 {
			return errors.New("data replication frame requires payload and forbids metadata")
		}
		if f.StoreGeneration == 0 || f.StoreSequenceFirst == 0 || f.StoreSequenceLast < f.StoreSequenceFirst {
			return errors.New("data replication frame has an invalid store generation or sequence range")
		}
		if (f.Store == StoreAudit || f.Store == StoreGovernance) && f.StoreGeneration != 1 {
			return errors.New("audit and governance frames must use store generation 1")
		}
	case KindLedgerRoll:
		if f.Store != StoreNone || len(f.Payload) != 0 || len(f.Metadata) != LedgerRollMetadataBytes ||
			f.StoreGeneration != 0 || f.StoreSequenceFirst != 0 || f.StoreSequenceLast != 0 {
			return errors.New("ledger-roll frame has an invalid shape")
		}
		if _, err := DecodeLedgerRollMetadata(f.Metadata); err != nil {
			return err
		}
	case KindLeadershipEstablished:
		if f.Store != StoreNone || len(f.Payload) != 0 || len(f.Metadata) != 0 ||
			f.StoreGeneration != 0 || f.StoreSequenceFirst != 0 || f.StoreSequenceLast != 0 {
			return errors.New("leadership-established frame has an invalid shape")
		}
	case KindSchemaBoundary:
		if f.Store != StoreNone || len(f.Payload) != 0 || len(f.Metadata) != SchemaBoundaryMetadataBytes ||
			f.StoreGeneration != 0 || f.StoreSequenceFirst != 0 || f.StoreSequenceLast != 0 {
			return errors.New("schema-boundary frame has an invalid shape")
		}
		if _, err := DecodeSchemaBoundaryMetadata(f.Metadata); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown replication frame kind %d", f.Kind)
	}
	return nil
}

func (f Frame) MarshalBinary() ([]byte, error) {
	encoded, _, err := f.MarshalBinaryAndDigest()
	return encoded, err
}

// MarshalBinaryAndDigest returns the digest that the corresponding ordering
// record must persist. Keeping both outputs from one encoding prevents a
// caller from accidentally hashing a different representation.
func (f Frame) MarshalBinaryAndDigest() ([]byte, [sha256.Size]byte, error) {
	if err := f.Validate(); err != nil {
		return nil, [sha256.Size]byte{}, err
	}
	bodyLength := frameBodyFixedBytes + len(f.ClusterID) + len(f.Incarnation) + len(f.Metadata) + len(f.Payload)
	encoded := make([]byte, 4+bodyLength)
	binary.BigEndian.PutUint32(encoded[0:4], uint32(bodyLength))
	body := encoded[4:]
	copy(body[0:8], frameMagic[:])
	binary.BigEndian.PutUint16(body[8:10], ProtocolVersion)
	binary.BigEndian.PutUint16(body[10:12], uint16(f.Kind))
	binary.BigEndian.PutUint16(body[12:14], uint16(f.Store))
	// body[14:16] is the reserved zero field.
	binary.BigEndian.PutUint64(body[16:24], f.Index)
	binary.BigEndian.PutUint64(body[24:32], f.Term)
	binary.BigEndian.PutUint64(body[32:40], f.ConfirmedIndex)
	binary.BigEndian.PutUint64(body[40:48], f.StoreGeneration)
	binary.BigEndian.PutUint64(body[48:56], f.StoreSequenceFirst)
	binary.BigEndian.PutUint64(body[56:64], f.StoreSequenceLast)
	binary.BigEndian.PutUint16(body[64:66], uint16(len(f.ClusterID)))
	binary.BigEndian.PutUint16(body[66:68], uint16(len(f.Incarnation)))
	binary.BigEndian.PutUint32(body[68:72], uint32(len(f.Metadata)))
	binary.BigEndian.PutUint32(body[72:76], uint32(len(f.Payload)))
	offset := frameBodyFixedBytes
	copy(body[offset:], f.ClusterID)
	offset += len(f.ClusterID)
	copy(body[offset:], f.Incarnation)
	offset += len(f.Incarnation)
	copy(body[offset:], f.Metadata)
	offset += len(f.Metadata)
	copy(body[offset:], f.Payload)
	digest := frameDigest(body)
	copy(body[frameDigestOffset:frameDigestOffset+sha256.Size], digest[:])
	return encoded, digest, nil
}

func UnmarshalFrame(encoded []byte) (Frame, error) {
	frame, _, err := UnmarshalFrameAndDigest(encoded)
	return frame, err
}

// UnmarshalFrameAndDigest returns only after the embedded digest and shape have
// both been verified.
func UnmarshalFrameAndDigest(encoded []byte) (Frame, [sha256.Size]byte, error) {
	if len(encoded) < 4+frameBodyFixedBytes {
		return Frame{}, [sha256.Size]byte{}, errors.New("replication frame is shorter than its fixed header")
	}
	declared := int(binary.BigEndian.Uint32(encoded[0:4]))
	if declared != len(encoded)-4 {
		return Frame{}, [sha256.Size]byte{}, fmt.Errorf("replication frame length is %d, declared %d", len(encoded)-4, declared)
	}
	if declared > frameBodyFixedBytes+2*MaxIdentityBytes+MaxMetadataBytes+MaxPayloadBytes {
		return Frame{}, [sha256.Size]byte{}, errors.New("replication frame exceeds the version-1 size bound")
	}
	body := encoded[4:]
	if !bytes.Equal(body[0:8], frameMagic[:]) {
		return Frame{}, [sha256.Size]byte{}, errors.New("replication frame magic is invalid")
	}
	if version := binary.BigEndian.Uint16(body[8:10]); version != ProtocolVersion {
		return Frame{}, [sha256.Size]byte{}, fmt.Errorf("unsupported replication frame version %d", version)
	}
	if binary.BigEndian.Uint16(body[14:16]) != 0 {
		return Frame{}, [sha256.Size]byte{}, errors.New("replication frame reserved field is non-zero")
	}
	clusterLength := int(binary.BigEndian.Uint16(body[64:66]))
	incarnationLength := int(binary.BigEndian.Uint16(body[66:68]))
	metadataLength := int(binary.BigEndian.Uint32(body[68:72]))
	payloadLength := int(binary.BigEndian.Uint32(body[72:76]))
	if clusterLength > MaxIdentityBytes || incarnationLength > MaxIdentityBytes ||
		metadataLength > MaxMetadataBytes || payloadLength > MaxPayloadBytes {
		return Frame{}, [sha256.Size]byte{}, errors.New("replication frame declares an excessive field length")
	}
	variableLength := clusterLength + incarnationLength + metadataLength + payloadLength
	if variableLength != len(body)-frameBodyFixedBytes {
		return Frame{}, [sha256.Size]byte{}, errors.New("replication frame field lengths do not consume the record")
	}
	wantDigest := append([]byte(nil), body[frameDigestOffset:frameDigestOffset+sha256.Size]...)
	gotDigest := frameDigest(body)
	if !bytes.Equal(wantDigest, gotDigest[:]) {
		return Frame{}, [sha256.Size]byte{}, errors.New("replication frame digest mismatch")
	}
	offset := frameBodyFixedBytes
	take := func(length int) []byte {
		value := append([]byte(nil), body[offset:offset+length]...)
		offset += length
		return value
	}
	frame := Frame{
		Kind:               Kind(binary.BigEndian.Uint16(body[10:12])),
		Store:              Store(binary.BigEndian.Uint16(body[12:14])),
		Index:              binary.BigEndian.Uint64(body[16:24]),
		Term:               binary.BigEndian.Uint64(body[24:32]),
		ConfirmedIndex:     binary.BigEndian.Uint64(body[32:40]),
		StoreGeneration:    binary.BigEndian.Uint64(body[40:48]),
		StoreSequenceFirst: binary.BigEndian.Uint64(body[48:56]),
		StoreSequenceLast:  binary.BigEndian.Uint64(body[56:64]),
		ClusterID:          string(take(clusterLength)),
		Incarnation:        string(take(incarnationLength)),
		Metadata:           take(metadataLength),
		Payload:            take(payloadLength),
	}
	if err := frame.Validate(); err != nil {
		return Frame{}, [sha256.Size]byte{}, err
	}
	return frame, gotDigest, nil
}

func frameDigest(body []byte) [sha256.Size]byte {
	hash := sha256.New()
	hash.Write(frameDomain)
	hash.Write(body[:frameDigestOffset])
	hash.Write(make([]byte, sha256.Size))
	hash.Write(body[frameDigestOffset+sha256.Size:])
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}

const LedgerRollMetadataBytes = 5*8 + 3*sha256.Size + 8

type LedgerRollMetadata struct {
	Generation    uint64
	FirstSequence uint64
	LastSequence  uint64
	Length        uint64
	EndEpoch      uint64
	StartHash     [sha256.Size]byte
	EndHash       [sha256.Size]byte
	PlainChecksum [sha256.Size]byte
	SealedAtUnix  int64
}

func (m LedgerRollMetadata) MarshalBinary() ([]byte, error) {
	if m.Generation == 0 || m.Generation == ^uint64(0) || m.FirstSequence == 0 || m.LastSequence < m.FirstSequence || m.Length == 0 || m.EndEpoch == 0 {
		return nil, errors.New("ledger-roll metadata has invalid generation, sequence, length or epoch")
	}
	encoded := make([]byte, LedgerRollMetadataBytes)
	values := []uint64{m.Generation, m.FirstSequence, m.LastSequence, m.Length, m.EndEpoch}
	offset := 0
	for _, value := range values {
		binary.BigEndian.PutUint64(encoded[offset:offset+8], value)
		offset += 8
	}
	copy(encoded[offset:offset+sha256.Size], m.StartHash[:])
	offset += sha256.Size
	copy(encoded[offset:offset+sha256.Size], m.EndHash[:])
	offset += sha256.Size
	copy(encoded[offset:offset+sha256.Size], m.PlainChecksum[:])
	offset += sha256.Size
	binary.BigEndian.PutUint64(encoded[offset:offset+8], uint64(m.SealedAtUnix))
	return encoded, nil
}

func DecodeLedgerRollMetadata(encoded []byte) (LedgerRollMetadata, error) {
	if len(encoded) != LedgerRollMetadataBytes {
		return LedgerRollMetadata{}, errors.New("ledger-roll metadata length is invalid")
	}
	metadata := LedgerRollMetadata{
		Generation:    binary.BigEndian.Uint64(encoded[0:8]),
		FirstSequence: binary.BigEndian.Uint64(encoded[8:16]),
		LastSequence:  binary.BigEndian.Uint64(encoded[16:24]),
		Length:        binary.BigEndian.Uint64(encoded[24:32]),
		EndEpoch:      binary.BigEndian.Uint64(encoded[32:40]),
	}
	offset := 40
	copy(metadata.StartHash[:], encoded[offset:offset+sha256.Size])
	offset += sha256.Size
	copy(metadata.EndHash[:], encoded[offset:offset+sha256.Size])
	offset += sha256.Size
	copy(metadata.PlainChecksum[:], encoded[offset:offset+sha256.Size])
	offset += sha256.Size
	metadata.SealedAtUnix = int64(binary.BigEndian.Uint64(encoded[offset : offset+8]))
	if _, err := metadata.MarshalBinary(); err != nil {
		return LedgerRollMetadata{}, err
	}
	return metadata, nil
}

const SchemaBoundaryMetadataBytes = 8

type SchemaBoundaryMetadata struct {
	From uint32
	To   uint32
}

func (m SchemaBoundaryMetadata) MarshalBinary() ([]byte, error) {
	if m.From == 0 || m.To != m.From+1 {
		return nil, errors.New("schema boundary must advance exactly one version")
	}
	encoded := make([]byte, SchemaBoundaryMetadataBytes)
	binary.BigEndian.PutUint32(encoded[0:4], m.From)
	binary.BigEndian.PutUint32(encoded[4:8], m.To)
	return encoded, nil
}

func DecodeSchemaBoundaryMetadata(encoded []byte) (SchemaBoundaryMetadata, error) {
	if len(encoded) != SchemaBoundaryMetadataBytes {
		return SchemaBoundaryMetadata{}, errors.New("schema-boundary metadata length is invalid")
	}
	metadata := SchemaBoundaryMetadata{From: binary.BigEndian.Uint32(encoded[0:4]), To: binary.BigEndian.Uint32(encoded[4:8])}
	if _, err := metadata.MarshalBinary(); err != nil {
		return SchemaBoundaryMetadata{}, err
	}
	return metadata, nil
}
