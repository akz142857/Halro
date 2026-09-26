package replication

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	OrderingVersion           uint16 = 1
	orderingHeaderFixedBytes         = 240
	orderingRecordBodyBytes          = 304
	orderingHeaderMACOffset          = 16
	orderingRecordMACOffset          = 272
	orderingMetadataOffset           = 96
	orderingPreviousMACOffset        = 240
)

var (
	orderingMagic        = [8]byte{'H', 'L', 'R', 'O', 'R', 'D', '0', '1'}
	orderingHeaderDomain = []byte("halro:cluster:v1\x00ordering-header\x00")
	orderingRecordDomain = []byte("halro:cluster:v1\x00ordering\x00")
)

type OrderingHeader struct {
	ClusterID   string
	Incarnation string
	// StoreCursors authenticate the native store positions at replication
	// index zero: either pre-existing Standalone history or a seeded snapshot.
	StoreCursors [4]StoreCursor
	// StoreHeads bind those positions to the native authenticated histories.
	// A cursor alone is not an identity: two valid chains can end at 1/100.
	StoreHeads [4][sha256.Size]byte
}

func (h OrderingHeader) MarshalBinary(key []byte) ([]byte, error) {
	if len(key) != sha256.Size {
		return nil, errors.New("ordering journal key must be 32 bytes")
	}
	if len(h.ClusterID) == 0 || len(h.ClusterID) > MaxIdentityBytes ||
		len(h.Incarnation) == 0 || len(h.Incarnation) > MaxIdentityBytes {
		return nil, fmt.Errorf("ordering journal identities must be 1-%d bytes", MaxIdentityBytes)
	}
	if err := validateBaselineCursors(h.StoreCursors); err != nil {
		return nil, err
	}
	if err := validateBaselineHeads(h.StoreCursors, h.StoreHeads); err != nil {
		return nil, err
	}
	bodyLength := orderingHeaderFixedBytes + len(h.ClusterID) + len(h.Incarnation)
	encoded := make([]byte, 4+bodyLength)
	binary.BigEndian.PutUint32(encoded[:4], uint32(bodyLength))
	body := encoded[4:]
	copy(body[0:8], orderingMagic[:])
	binary.BigEndian.PutUint16(body[8:10], OrderingVersion)
	// body[10:12] is reserved.
	binary.BigEndian.PutUint16(body[12:14], uint16(len(h.ClusterID)))
	binary.BigEndian.PutUint16(body[14:16], uint16(len(h.Incarnation)))
	for store := StoreLedger; store <= StoreMetadata; store++ {
		offset := 48 + int(store-StoreLedger)*16
		cursor := h.StoreCursors[store-StoreLedger]
		binary.BigEndian.PutUint64(body[offset:offset+8], cursor.Generation)
		binary.BigEndian.PutUint64(body[offset+8:offset+16], cursor.Sequence)
		headOffset := 112 + int(store-StoreLedger)*sha256.Size
		copy(body[headOffset:headOffset+sha256.Size], h.StoreHeads[store-StoreLedger][:])
	}
	copy(body[orderingHeaderFixedBytes:], h.ClusterID)
	copy(body[orderingHeaderFixedBytes+len(h.ClusterID):], h.Incarnation)
	mac := orderingMAC(key, orderingHeaderDomain, nil, nil, body, orderingHeaderMACOffset)
	copy(body[orderingHeaderMACOffset:orderingHeaderMACOffset+sha256.Size], mac[:])
	return encoded, nil
}

func UnmarshalOrderingHeader(encoded, key []byte) (OrderingHeader, error) {
	if len(key) != sha256.Size {
		return OrderingHeader{}, errors.New("ordering journal key must be 32 bytes")
	}
	if len(encoded) < 4+orderingHeaderFixedBytes {
		return OrderingHeader{}, errors.New("ordering journal header is truncated")
	}
	declared := int(binary.BigEndian.Uint32(encoded[:4]))
	if declared != len(encoded)-4 {
		return OrderingHeader{}, errors.New("ordering journal header length is not canonical")
	}
	body := encoded[4:]
	if !bytes.Equal(body[0:8], orderingMagic[:]) {
		return OrderingHeader{}, errors.New("ordering journal magic is invalid")
	}
	if version := binary.BigEndian.Uint16(body[8:10]); version != OrderingVersion {
		return OrderingHeader{}, fmt.Errorf("unsupported ordering journal version %d", version)
	}
	if binary.BigEndian.Uint16(body[10:12]) != 0 {
		return OrderingHeader{}, errors.New("ordering journal header reserved field is non-zero")
	}
	clusterLength := int(binary.BigEndian.Uint16(body[12:14]))
	incarnationLength := int(binary.BigEndian.Uint16(body[14:16]))
	if clusterLength == 0 || clusterLength > MaxIdentityBytes || incarnationLength == 0 || incarnationLength > MaxIdentityBytes ||
		orderingHeaderFixedBytes+clusterLength+incarnationLength != len(body) {
		return OrderingHeader{}, errors.New("ordering journal header identity lengths are invalid")
	}
	want := append([]byte(nil), body[orderingHeaderMACOffset:orderingHeaderMACOffset+sha256.Size]...)
	got := orderingMAC(key, orderingHeaderDomain, nil, nil, body, orderingHeaderMACOffset)
	if !hmac.Equal(want, got[:]) {
		return OrderingHeader{}, errors.New("ordering journal header MAC mismatch")
	}
	header := OrderingHeader{
		ClusterID:   string(body[orderingHeaderFixedBytes : orderingHeaderFixedBytes+clusterLength]),
		Incarnation: string(body[orderingHeaderFixedBytes+clusterLength:]),
	}
	for store := StoreLedger; store <= StoreMetadata; store++ {
		offset := 48 + int(store-StoreLedger)*16
		header.StoreCursors[store-StoreLedger] = StoreCursor{
			Generation: binary.BigEndian.Uint64(body[offset : offset+8]),
			Sequence:   binary.BigEndian.Uint64(body[offset+8 : offset+16]),
		}
		headOffset := 112 + int(store-StoreLedger)*sha256.Size
		copy(header.StoreHeads[store-StoreLedger][:], body[headOffset:headOffset+sha256.Size])
	}
	if err := validateBaselineCursors(header.StoreCursors); err != nil {
		return OrderingHeader{}, err
	}
	if err := validateBaselineHeads(header.StoreCursors, header.StoreHeads); err != nil {
		return OrderingHeader{}, err
	}
	return header, nil
}

func validateBaselineCursors(cursors [4]StoreCursor) error {
	for store := StoreLedger; store <= StoreMetadata; store++ {
		cursor := cursors[store-StoreLedger]
		if cursor.Generation == 0 && cursor.Sequence != 0 {
			return fmt.Errorf("ordering baseline store %d has sequence without generation", store)
		}
		if (store == StoreAudit || store == StoreGovernance) && cursor.Generation > 1 {
			return fmt.Errorf("ordering baseline store %d has invalid generation", store)
		}
	}
	return nil
}

func validateBaselineHeads(cursors [4]StoreCursor, heads [4][sha256.Size]byte) error {
	for store := StoreLedger; store <= StoreMetadata; store++ {
		cursor := cursors[store-StoreLedger]
		hasHistory := cursor.Sequence > 0 || store == StoreMetadata && cursor.Generation > 0
		hasHead := heads[store-StoreLedger] != ([sha256.Size]byte{})
		if hasHistory && !hasHead {
			return fmt.Errorf("ordering baseline store %d has history without an authenticated head", store)
		}
		if !hasHistory && hasHead {
			return fmt.Errorf("ordering baseline store %d has an authenticated head without history", store)
		}
	}
	return nil
}

type OrderingRecord struct {
	Kind               Kind
	Store              Store
	Index              uint64
	Term               uint64
	ConfirmedIndex     uint64
	StoreGeneration    uint64
	StoreSequenceFirst uint64
	StoreSequenceLast  uint64
	// Metadata persists the bounded control-frame envelope needed to recreate
	// the exact encoded frame after a crash. Data records never carry it.
	Metadata    []byte
	FrameDigest [sha256.Size]byte
	PreviousMAC [sha256.Size]byte
	MAC         [sha256.Size]byte
}

func (r OrderingRecord) validate() error {
	if r.Index == 0 || r.Term == 0 {
		return errors.New("ordering record index and term must be positive")
	}
	if r.ConfirmedIndex >= r.Index {
		return errors.New("ordering record confirmed index must precede its own index")
	}
	if r.FrameDigest == ([sha256.Size]byte{}) {
		return errors.New("ordering record frame digest is empty")
	}
	switch r.Kind {
	case KindData:
		if r.Store < StoreLedger || r.Store > StoreMetadata || r.StoreGeneration == 0 || r.StoreSequenceFirst == 0 || r.StoreSequenceLast < r.StoreSequenceFirst || len(r.Metadata) != 0 {
			return errors.New("ordering data record has an invalid store, generation or sequence range")
		}
		if (r.Store == StoreAudit || r.Store == StoreGovernance) && r.StoreGeneration != 1 {
			return errors.New("audit and governance ordering records must use store generation 1")
		}
	case KindLedgerRoll:
		if r.Store != StoreNone || r.StoreGeneration != 0 || r.StoreSequenceFirst != 0 || r.StoreSequenceLast != 0 {
			return errors.New("ordering control record has an invalid store or sequence range")
		}
		if len(r.Metadata) != LedgerRollMetadataBytes {
			return errors.New("ledger-roll ordering metadata length is invalid")
		}
		if _, err := DecodeLedgerRollMetadata(r.Metadata); err != nil {
			return err
		}
	case KindLeadershipEstablished:
		if r.Store != StoreNone || r.StoreGeneration != 0 || r.StoreSequenceFirst != 0 || r.StoreSequenceLast != 0 || len(r.Metadata) != 0 {
			return errors.New("leadership ordering record has an invalid shape")
		}
	case KindSchemaBoundary:
		if r.Store != StoreNone || r.StoreGeneration != 0 || r.StoreSequenceFirst != 0 || r.StoreSequenceLast != 0 || len(r.Metadata) != SchemaBoundaryMetadataBytes {
			return errors.New("schema-boundary ordering record has an invalid shape")
		}
		if _, err := DecodeSchemaBoundaryMetadata(r.Metadata); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown ordering record kind %d", r.Kind)
	}
	return nil
}

func (r OrderingRecord) MarshalBinary(key []byte, clusterID, incarnation string) ([]byte, error) {
	encoded, _, err := r.MarshalBinaryAndMAC(key, clusterID, incarnation)
	return encoded, err
}

// MarshalBinaryAndMAC returns both the authenticated record and the MAC that
// the next record must carry as PreviousMAC. Returning the chain head avoids
// making an appender parse bytes it just encoded or copy a magic offset.
func (r OrderingRecord) MarshalBinaryAndMAC(key []byte, clusterID, incarnation string) ([]byte, [sha256.Size]byte, error) {
	if len(key) != sha256.Size {
		return nil, [sha256.Size]byte{}, errors.New("ordering journal key must be 32 bytes")
	}
	if len(clusterID) == 0 || len(clusterID) > MaxIdentityBytes || len(incarnation) == 0 || len(incarnation) > MaxIdentityBytes {
		return nil, [sha256.Size]byte{}, errors.New("ordering record identity is invalid")
	}
	if err := r.validate(); err != nil {
		return nil, [sha256.Size]byte{}, err
	}
	encoded := make([]byte, 4+orderingRecordBodyBytes)
	binary.BigEndian.PutUint32(encoded[:4], orderingRecordBodyBytes)
	body := encoded[4:]
	binary.BigEndian.PutUint16(body[0:2], OrderingVersion)
	binary.BigEndian.PutUint16(body[2:4], uint16(r.Kind))
	binary.BigEndian.PutUint16(body[4:6], uint16(r.Store))
	// body[6:8] is reserved.
	binary.BigEndian.PutUint64(body[8:16], r.Index)
	binary.BigEndian.PutUint64(body[16:24], r.Term)
	binary.BigEndian.PutUint64(body[24:32], r.ConfirmedIndex)
	binary.BigEndian.PutUint64(body[32:40], r.StoreGeneration)
	binary.BigEndian.PutUint64(body[40:48], r.StoreSequenceFirst)
	binary.BigEndian.PutUint64(body[48:56], r.StoreSequenceLast)
	binary.BigEndian.PutUint16(body[56:58], uint16(len(r.Metadata)))
	// body[58:64] is reserved.
	copy(body[64:96], r.FrameDigest[:])
	copy(body[orderingMetadataOffset:orderingMetadataOffset+LedgerRollMetadataBytes], r.Metadata)
	copy(body[orderingPreviousMACOffset:orderingRecordMACOffset], r.PreviousMAC[:])
	mac := orderingMAC(key, orderingRecordDomain, []byte(clusterID), []byte(incarnation), body, orderingRecordMACOffset)
	copy(body[orderingRecordMACOffset:orderingRecordMACOffset+sha256.Size], mac[:])
	return encoded, mac, nil
}

func UnmarshalOrderingRecord(encoded, key []byte, clusterID, incarnation string, expectedPrevious [sha256.Size]byte) (OrderingRecord, error) {
	if len(key) != sha256.Size {
		return OrderingRecord{}, errors.New("ordering journal key must be 32 bytes")
	}
	if len(clusterID) == 0 || len(clusterID) > MaxIdentityBytes || len(incarnation) == 0 || len(incarnation) > MaxIdentityBytes {
		return OrderingRecord{}, errors.New("ordering record identity is invalid")
	}
	if len(encoded) != 4+orderingRecordBodyBytes || binary.BigEndian.Uint32(encoded[:4]) != orderingRecordBodyBytes {
		return OrderingRecord{}, errors.New("ordering record length is not canonical")
	}
	body := encoded[4:]
	if version := binary.BigEndian.Uint16(body[0:2]); version != OrderingVersion {
		return OrderingRecord{}, fmt.Errorf("unsupported ordering record version %d", version)
	}
	if binary.BigEndian.Uint16(body[6:8]) != 0 || !allZero(body[58:64]) {
		return OrderingRecord{}, errors.New("ordering record reserved field is non-zero")
	}
	metadataLength := int(binary.BigEndian.Uint16(body[56:58]))
	if metadataLength > LedgerRollMetadataBytes || !allZero(body[orderingMetadataOffset+metadataLength:orderingPreviousMACOffset]) {
		return OrderingRecord{}, errors.New("ordering record metadata length or padding is not canonical")
	}
	record := OrderingRecord{
		Kind:               Kind(binary.BigEndian.Uint16(body[2:4])),
		Store:              Store(binary.BigEndian.Uint16(body[4:6])),
		Index:              binary.BigEndian.Uint64(body[8:16]),
		Term:               binary.BigEndian.Uint64(body[16:24]),
		ConfirmedIndex:     binary.BigEndian.Uint64(body[24:32]),
		StoreGeneration:    binary.BigEndian.Uint64(body[32:40]),
		StoreSequenceFirst: binary.BigEndian.Uint64(body[40:48]),
		StoreSequenceLast:  binary.BigEndian.Uint64(body[48:56]),
		Metadata:           append([]byte(nil), body[orderingMetadataOffset:orderingMetadataOffset+metadataLength]...),
	}
	copy(record.FrameDigest[:], body[64:96])
	copy(record.PreviousMAC[:], body[orderingPreviousMACOffset:orderingRecordMACOffset])
	copy(record.MAC[:], body[orderingRecordMACOffset:orderingRecordBodyBytes])
	if record.PreviousMAC != expectedPrevious {
		return OrderingRecord{}, errors.New("ordering record does not continue the expected MAC chain")
	}
	want := orderingMAC(key, orderingRecordDomain, []byte(clusterID), []byte(incarnation), body, orderingRecordMACOffset)
	if !hmac.Equal(record.MAC[:], want[:]) {
		return OrderingRecord{}, errors.New("ordering record MAC mismatch")
	}
	if err := record.validate(); err != nil {
		return OrderingRecord{}, err
	}
	return record, nil
}

func orderingMAC(key, domain, clusterID, incarnation, body []byte, macOffset int) [sha256.Size]byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(domain)
	if clusterID != nil {
		var lengths [4]byte
		binary.BigEndian.PutUint16(lengths[0:2], uint16(len(clusterID)))
		binary.BigEndian.PutUint16(lengths[2:4], uint16(len(incarnation)))
		mac.Write(lengths[:])
		mac.Write(clusterID)
		mac.Write(incarnation)
	}
	mac.Write(body[:macOffset])
	mac.Write(make([]byte, sha256.Size))
	mac.Write(body[macOffset+sha256.Size:])
	var sum [sha256.Size]byte
	copy(sum[:], mac.Sum(nil))
	return sum
}
