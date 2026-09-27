package replication

import (
	"encoding/hex"
	"strings"
	"testing"
)

func testAcknowledgement(node string, index uint64) Acknowledgement {
	return Acknowledgement{
		ClusterID: "production-a", Incarnation: "inc_01", NodeID: node,
		Index: index, Term: 7, DurableIndex: index, AppliedIndex: index - 1,
	}
}

func TestAcknowledgementVersionOneGoldenEncoding(t *testing.T) {
	ack := testAcknowledgement("halro-1", 11)
	encoded, err := ack.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	const wantHex = "00000055484c5241434b303100010000000000000000000b0000000000000007000000000000000b000000000000000a000c000600070000000000000000000070726f64756374696f6e2d61696e635f303168616c726f2d31"
	if got := hex.EncodeToString(encoded); got != wantHex {
		t.Fatalf("version-1 acknowledgement fixture changed\n got: %s\nwant: %s", got, wantHex)
	}
	decoded, err := UnmarshalAcknowledgement(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != ack {
		t.Fatalf("decoded=%#v want=%#v", decoded, ack)
	}
}

func TestAcknowledgementRefusesNonCanonicalShapes(t *testing.T) {
	encoded, err := testAcknowledgement("halro-1", 11).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		edit func([]byte) []byte
		want string
	}{
		{name: "trailing", edit: func(value []byte) []byte { return append(value, 0) }, want: "length"},
		{name: "version", edit: func(value []byte) []byte { value[4+9] = 2; return value }, want: "unsupported"},
		{name: "reserved", edit: func(value []byte) []byte { value[4+10] = 1; return value }, want: "reserved"},
		{name: "watermark", edit: func(value []byte) []byte { value[4+19] = 12; return value }, want: "indexes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := test.edit(append([]byte(nil), encoded...))
			if _, err := UnmarshalAcknowledgement(candidate); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func testConfirmationFrame(index uint64, kind Kind) Frame {
	frame := Frame{Kind: kind, Store: StoreNone, Index: index, Term: 7, ClusterID: "production-a", Incarnation: "inc_01"}
	if kind == KindData {
		frame.Store = StoreLedger
		frame.StoreGeneration = 1
		frame.StoreSequenceFirst = index
		frame.StoreSequenceLast = index
		frame.Payload = []byte{byte(index)}
	}
	return frame
}

func TestConfirmationTrackerRequiresAnchorAndAdvancesCumulativePrefix(t *testing.T) {
	tracker, err := NewConfirmationTracker("production-a", "inc_01", 7, 10, []string{"halro-1", "halro-2"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.Register(testConfirmationFrame(11, KindData)); err == nil || !strings.Contains(err.Error(), "establish leadership") {
		t.Fatalf("missing-anchor error=%v", err)
	}
	if err := tracker.Register(testConfirmationFrame(11, KindLeadershipEstablished)); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Register(testConfirmationFrame(12, KindData)); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Register(testConfirmationFrame(13, KindData)); err != nil {
		t.Fatal(err)
	}
	ack := testAcknowledgement("halro-1", 13)
	if confirmed, err := tracker.Acknowledge(ack); err != nil || confirmed != 13 {
		t.Fatalf("confirmed=%d err=%v", confirmed, err)
	}
	if confirmed, err := tracker.Acknowledge(ack); err != nil || confirmed != 13 {
		t.Fatalf("duplicate confirmed=%d err=%v", confirmed, err)
	}
}

func TestConfirmationTrackerRefusesWrongTermMemberAndFutureACK(t *testing.T) {
	tracker, err := NewConfirmationTracker("production-a", "inc_01", 7, 0, []string{"halro-1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.Register(testConfirmationFrame(1, KindLeadershipEstablished)); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		ack  Acknowledgement
		want string
	}{
		{name: "old_term", ack: func() Acknowledgement { value := testAcknowledgement("halro-1", 1); value.Term = 6; return value }(), want: "term"},
		{name: "unknown_peer", ack: testAcknowledgement("halro-2", 1), want: "unconfigured"},
		{name: "future", ack: testAcknowledgement("halro-1", 2), want: "future"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := tracker.Acknowledge(test.ack); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestConfirmationTrackerResumesAnEstablishedTermWithoutSecondAnchor(t *testing.T) {
	tracker, err := NewConfirmationTracker("production-a", "inc_01", 7, 11, []string{"halro-1"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.Register(testConfirmationFrame(12, KindData)); err != nil {
		t.Fatal(err)
	}
}
