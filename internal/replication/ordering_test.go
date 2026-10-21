package replication

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestOrderingHeaderAndRecordAuthenticateIdentityAndChain(t *testing.T) {
	key := make([]byte, sha256.Size)
	for index := range key {
		key[index] = byte(index + 1)
	}
	header := OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}
	encodedHeader, err := header.MarshalBinary(key)
	if err != nil {
		t.Fatal(err)
	}
	decodedHeader, err := UnmarshalOrderingHeader(encodedHeader, key)
	if err != nil || decodedHeader != header {
		t.Fatalf("header=%#v err=%v", decodedHeader, err)
	}

	digest := sha256.Sum256([]byte("frame"))
	first := OrderingRecord{Kind: KindData, Store: StoreLedger, Index: 1, Term: 1, StoreGeneration: 2, StoreSequenceFirst: 7, StoreSequenceLast: 9, FrameDigest: digest}
	encodedFirst, err := first.MarshalBinary(key, header.ClusterID, header.Incarnation)
	if err != nil {
		t.Fatal(err)
	}
	decodedFirst, err := UnmarshalOrderingRecord(encodedFirst, key, header.ClusterID, header.Incarnation, [sha256.Size]byte{})
	if err != nil {
		t.Fatal(err)
	}
	second := OrderingRecord{Kind: KindLeadershipEstablished, Store: StoreNone, Index: 2, Term: 2, FrameDigest: sha256.Sum256([]byte("leader")), PreviousMAC: decodedFirst.MAC}
	encodedSecond, err := second.MarshalBinary(key, header.ClusterID, header.Incarnation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalOrderingRecord(encodedSecond, key, header.ClusterID, header.Incarnation, decodedFirst.MAC); err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalOrderingRecord(encodedSecond, key, header.ClusterID, "inc_other", decodedFirst.MAC); err == nil || !strings.Contains(err.Error(), "MAC mismatch") {
		t.Fatalf("wrong incarnation error=%v", err)
	}
	if _, err := UnmarshalOrderingRecord(encodedSecond, key, header.ClusterID, header.Incarnation, [sha256.Size]byte{}); err == nil || !strings.Contains(err.Error(), "expected MAC chain") {
		t.Fatalf("wrong predecessor error=%v", err)
	}
}

func TestOrderingVersionOneGoldenRecord(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	record := OrderingRecord{
		Kind: KindData, Store: StoreMetadata, Index: 9, Term: 3,
		StoreGeneration:    4,
		StoreSequenceFirst: 41, StoreSequenceLast: 42,
		FrameDigest: sha256.Sum256([]byte("frame")),
	}
	encoded, err := record.MarshalBinary(key, "c", "i")
	if err != nil {
		t.Fatal(err)
	}
	const wantHex = "00000130000100010004000000000000000000090000000000000003000000000000000000000000000000040000000000000029000000000000002a00000000000000009dff50df08c635815f4b19da10f756605a34a79a48d4ba48712782502975a70e0000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000e48e8957546964aca6b17aa3a88406860a9c4c0784a666cd4b41668daed30395"
	if got := hex.EncodeToString(encoded); got != wantHex {
		t.Fatalf("version-1 ordering fixture changed\n got: %s\nwant: %s", got, wantHex)
	}
}

func TestOrderingVersionOneGoldenHeader(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	encoded, err := (OrderingHeader{ClusterID: "c", Incarnation: "i"}).MarshalBinary(key)
	if err != nil {
		t.Fatal(err)
	}
	const wantHex = "000000f2484c524f5244303100010000000100012c4d087698a40c718dc6d4058dc7d6b86ca8099ed6b815b63c05513f7a07e5b60000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000006369"
	if got := hex.EncodeToString(encoded); got != wantHex {
		t.Fatalf("version-1 ordering header fixture changed\n got: %s\nwant: %s", got, wantHex)
	}
}

func TestOrderingHeaderAuthenticatesSeedBaselines(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	header := OrderingHeader{ClusterID: "cluster", Incarnation: "inc_1"}
	header.StoreCursors[StoreLedger-StoreLedger] = StoreCursor{Generation: 3, Sequence: 91}
	header.StoreCursors[StoreAudit-StoreLedger] = StoreCursor{Generation: 1, Sequence: 12}
	header.StoreCursors[StoreGovernance-StoreLedger] = StoreCursor{Generation: 1, Sequence: 4}
	header.StoreCursors[StoreMetadata-StoreLedger] = StoreCursor{Generation: 7, Sequence: 22}
	for index := range header.StoreHeads {
		header.StoreHeads[index] = sha256.Sum256([]byte{byte(index + 1)})
	}
	encoded, err := header.MarshalBinary(key)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalOrderingHeader(encoded, key)
	if err != nil || decoded != header {
		t.Fatalf("decoded=%#v err=%v", decoded, err)
	}
	encoded[len(encoded)-1] ^= 1
	if _, err := UnmarshalOrderingHeader(encoded, key); err == nil {
		t.Fatal("tampered baseline was accepted")
	}
}

func TestOrderingHeaderRequiresTheMetadataEpochHeaderHash(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	header := OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}
	header.StoreCursors[StoreMetadata-StoreLedger] = StoreCursor{Generation: 1}
	if _, err := header.MarshalBinary(key); err == nil || !strings.Contains(err.Error(), "authenticated head") {
		t.Fatalf("missing metadata epoch head error=%v", err)
	}
	header.StoreHeads[StoreMetadata-StoreLedger] = [32]byte{1}
	if _, err := header.MarshalBinary(key); err != nil {
		t.Fatal(err)
	}
}

func TestOrderingRecordPreservesExactControlEnvelope(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	metadata, err := (SchemaBoundaryMetadata{From: 38, To: 39}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	record := OrderingRecord{
		Kind: KindSchemaBoundary, Store: StoreNone, Index: 7, Term: 2, ConfirmedIndex: 5,
		Metadata: metadata, FrameDigest: sha256.Sum256([]byte("boundary-frame")),
	}
	encoded, err := record.MarshalBinary(key, "cluster", "inc_1")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalOrderingRecord(encoded, key, "cluster", "inc_1", [sha256.Size]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ConfirmedIndex != 5 || string(decoded.Metadata) != string(metadata) {
		t.Fatalf("decoded control envelope=%#v", decoded)
	}
}

func TestOrderingAppenderReceivesNextChainHead(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	record := OrderingRecord{
		Kind: KindLeadershipEstablished, Store: StoreNone, Index: 1, Term: 1,
		FrameDigest: sha256.Sum256([]byte("leader")),
	}
	encoded, head, err := record.MarshalBinaryAndMAC(key, "cluster", "inc_1")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalOrderingRecord(encoded, key, "cluster", "inc_1", [sha256.Size]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if head != decoded.MAC {
		t.Fatalf("returned head %x != encoded MAC %x", head, decoded.MAC)
	}
}

func TestOrderingRefusesTamperingAndUnknownShape(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	record := OrderingRecord{
		Kind: KindData, Store: StoreAudit, Index: 1, Term: 1,
		StoreGeneration:    1,
		StoreSequenceFirst: 1, StoreSequenceLast: 1,
		FrameDigest: sha256.Sum256([]byte("frame")),
	}
	encoded, err := record.MarshalBinary(key, "cluster", "inc_1")
	if err != nil {
		t.Fatal(err)
	}
	encoded[4+40] ^= 1
	if _, err := UnmarshalOrderingRecord(encoded, key, "cluster", "inc_1", [sha256.Size]byte{}); err == nil || !strings.Contains(err.Error(), "MAC mismatch") {
		t.Fatalf("tamper error=%v", err)
	}

	record.Kind = Kind(99)
	if _, err := record.MarshalBinary(key, "cluster", "inc_1"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown kind error=%v", err)
	}
	if _, err := UnmarshalOrderingRecord(make([]byte, 4+orderingRecordBodyBytes), key, "", "inc_1", [sha256.Size]byte{}); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("empty identity error=%v", err)
	}
	record.Kind = KindData
	record.Store = StoreAudit
	record.StoreGeneration = 2
	if _, err := record.MarshalBinary(key, "cluster", "inc_1"); err == nil || !strings.Contains(err.Error(), "generation 1") {
		t.Fatalf("audit generation error=%v", err)
	}
}
