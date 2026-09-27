package replication

import (
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

func TestFrameVersionOneGoldenEncoding(t *testing.T) {
	frame := Frame{
		Kind: KindData, Store: StoreLedger,
		Index: 9, Term: 3, ConfirmedIndex: 8,
		StoreGeneration:    2,
		StoreSequenceFirst: 41, StoreSequenceLast: 42,
		ClusterID: "c", Incarnation: "i", Payload: []byte("abc"),
	}
	encoded, digest, err := frame.MarshalBinaryAndDigest()
	if err != nil {
		t.Fatal(err)
	}
	const wantHex = "00000071484c52504c303031000100010001000000000000000000090000000000000003000000000000000800000000000000020000000000000029000000000000002a0001000100000000000000036c3737916269b5797dad3d1ff43038c6514971cbd462ebbdb1f432f3c6e7ecea6369616263"
	if got := hex.EncodeToString(encoded); got != wantHex {
		t.Fatalf("version-1 fixture changed\n got: %s\nwant: %s", got, wantHex)
	}
	decoded, decodedDigest, err := UnmarshalFrameAndDigest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, frame) {
		t.Fatalf("decoded=%#v want=%#v", decoded, frame)
	}
	if digest != decodedDigest {
		t.Fatalf("encoded digest %x != decoded digest %x", digest, decodedDigest)
	}
}

func TestFrameVersionOneGoldenControlEncodings(t *testing.T) {
	roll := LedgerRollMetadata{Generation: 2, FirstSequence: 3, LastSequence: 9, Length: 1024, EndEpoch: 5, SealedAtUnix: 1234}
	roll.StartHash[0], roll.EndHash[0], roll.PlainChecksum[0] = 1, 2, 3
	rollBytes, err := roll.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	boundaryBytes, err := (SchemaBoundaryMetadata{From: 38, To: 39}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		frame Frame
		want  string
	}{
		{name: "ledger_roll", frame: Frame{Kind: KindLedgerRoll, Index: 10, Term: 3, ConfirmedIndex: 8, ClusterID: "c", Incarnation: "i", Metadata: rollBytes}, want: "000000fe484c52504c3030310001000200000000000000000000000a0000000000000003000000000000000800000000000000000000000000000000000000000000000000010001000000900000000089558aed50b5ef8f1bc2c6b2fe27b1662122499f33e750f2a4a9fb2d93d16d7f63690000000000000002000000000000000300000000000000090000000000000400000000000000000501000000000000000000000000000000000000000000000000000000000000000200000000000000000000000000000000000000000000000000000000000000030000000000000000000000000000000000000000000000000000000000000000000000000004d2"},
		{name: "leadership_established", frame: Frame{Kind: KindLeadershipEstablished, Index: 11, Term: 4, ConfirmedIndex: 10, ClusterID: "c", Incarnation: "i"}, want: "0000006e484c52504c3030310001000300000000000000000000000b0000000000000004000000000000000a000000000000000000000000000000000000000000000000000100010000000000000000a5b5ab6de24c4b4504711edb3394617ac368fdae842dae6fc2ed29686dbf343a6369"},
		{name: "schema_boundary", frame: Frame{Kind: KindSchemaBoundary, Index: 12, Term: 4, ConfirmedIndex: 10, ClusterID: "c", Incarnation: "i", Metadata: boundaryBytes}, want: "00000076484c52504c3030310001000400000000000000000000000c0000000000000004000000000000000a000000000000000000000000000000000000000000000000000100010000000800000000150d031ff39f6e26cf05a06d2c048a5844dbc15420eb3461f9a6d46442f16a4663690000002600000027"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := test.frame.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(encoded); got != test.want {
				t.Fatalf("version-1 %s fixture changed\n got: %s\nwant: %s", test.name, got, test.want)
			}
		})
	}
}

func TestFrameRefusesCorruptionAndNonCanonicalLengths(t *testing.T) {
	encoded, err := (Frame{
		Kind: KindData, Store: StoreMetadata, Index: 1, Term: 1,
		StoreGeneration:    1,
		StoreSequenceFirst: 1, StoreSequenceLast: 1,
		ClusterID: "cluster", Incarnation: "inc_1", Payload: []byte("frame"),
	}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func([]byte) []byte
		want   string
	}{
		{name: "payload", mutate: func(value []byte) []byte { value[len(value)-1] ^= 1; return value }, want: "digest mismatch"},
		{name: "trailing", mutate: func(value []byte) []byte { return append(value, 0) }, want: "declared"},
		{name: "reserved", mutate: func(value []byte) []byte { value[4+14] = 1; return value }, want: "reserved"},
		{name: "version", mutate: func(value []byte) []byte { value[4+9] = 2; return value }, want: "unsupported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := append([]byte(nil), encoded...)
			candidate = test.mutate(candidate)
			if _, err := UnmarshalFrame(candidate); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestControlMetadataRoundTrips(t *testing.T) {
	roll := LedgerRollMetadata{Generation: 2, FirstSequence: 3, LastSequence: 9, Length: 1024, EndEpoch: 5, SealedAtUnix: 1234}
	roll.StartHash[0], roll.EndHash[0], roll.PlainChecksum[0] = 1, 2, 3
	encodedRoll, err := roll.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	decodedRoll, err := DecodeLedgerRollMetadata(encodedRoll)
	if err != nil || decodedRoll != roll {
		t.Fatalf("decoded=%#v err=%v", decodedRoll, err)
	}

	boundary := SchemaBoundaryMetadata{From: 38, To: 39}
	encodedBoundary, err := boundary.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	decodedBoundary, err := DecodeSchemaBoundaryMetadata(encodedBoundary)
	if err != nil || decodedBoundary != boundary {
		t.Fatalf("decoded=%#v err=%v", decodedBoundary, err)
	}
}

func TestFrameShapesFailClosed(t *testing.T) {
	tests := []Frame{
		{Kind: KindData, Store: StoreNone, Index: 1, Term: 1, ClusterID: "c", Incarnation: "i", Payload: []byte("x"), StoreSequenceFirst: 1, StoreSequenceLast: 1},
		{Kind: KindLeadershipEstablished, Store: StoreNone, Index: 1, Term: 1, ClusterID: "c", Incarnation: "i", Payload: []byte("x")},
		{Kind: KindSchemaBoundary, Store: StoreNone, Index: 1, Term: 1, ClusterID: "c", Incarnation: "i", Metadata: make([]byte, SchemaBoundaryMetadataBytes)},
	}
	for _, frame := range tests {
		if _, err := frame.MarshalBinary(); err == nil {
			t.Fatalf("invalid frame was encoded: %#v", frame)
		}
	}
}

func TestLedgerRollRejectsGenerationOverflow(t *testing.T) {
	if _, err := (LedgerRollMetadata{
		Generation: ^uint64(0), FirstSequence: 1, LastSequence: 1, Length: 1, EndEpoch: 1,
	}).MarshalBinary(); err == nil {
		t.Fatal("ledger roll accepted a generation that would wrap on the Replica")
	}
}
