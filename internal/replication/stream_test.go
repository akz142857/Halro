package replication

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

type oneByteReader struct{ reader io.Reader }

func (r oneByteReader) Read(buffer []byte) (int, error) {
	if len(buffer) > 1 {
		buffer = buffer[:1]
	}
	return r.reader.Read(buffer)
}

func TestStreamReadsFragmentedMixedPrimaryRecordsWithoutLosingBoundaries(t *testing.T) {
	frame, err := (Frame{
		Kind: KindData, Store: StoreAudit, Index: 1, Term: 1,
		StoreGeneration: 1, StoreSequenceFirst: 1, StoreSequenceLast: 1,
		ClusterID: "cluster", Incarnation: "incarnation", Payload: []byte("native frame"),
	}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	notice, err := (CommitNotice{
		ClusterID: "cluster", Incarnation: "incarnation", NodeID: "primary", Term: 1, ConfirmedIndex: 1,
	}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	stream := oneByteReader{reader: bytes.NewReader(append(append([]byte(nil), frame...), notice...))}
	first, err := ReadStreamRecord(stream, StreamPrimaryToReplica)
	if err != nil || first.Kind != StreamRecordFrame || !bytes.Equal(first.Encoded, frame) {
		t.Fatalf("first record kind=%d err=%v", first.Kind, err)
	}
	second, err := ReadStreamRecord(stream, StreamPrimaryToReplica)
	if err != nil || second.Kind != StreamRecordCommitNotice || !bytes.Equal(second.Encoded, notice) {
		t.Fatalf("second record kind=%d err=%v", second.Kind, err)
	}
}

func TestStreamAppliesTypeBoundBeforeReadingPeerDeclaredBody(t *testing.T) {
	header := make([]byte, streamPrefixBytes)
	binary.BigEndian.PutUint32(header[:4], uint32(frameBodyFixedBytes+MaxPayloadBytes))
	copy(header[4:], commitNoticeMagic[:])
	_, err := ReadStreamRecord(bytes.NewReader(header), StreamPrimaryToReplica)
	if err == nil || !strings.Contains(err.Error(), "commit-notice record length") {
		t.Fatalf("oversized commit notice error=%v", err)
	}
}

func TestStreamRejectsRecordsInTheWrongDirection(t *testing.T) {
	ack, err := (Acknowledgement{
		ClusterID: "cluster", Incarnation: "incarnation", NodeID: "replica",
		Index: 1, Term: 1, DurableIndex: 1,
	}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStreamRecord(bytes.NewReader(ack), StreamPrimaryToReplica); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("reverse ACK read error=%v", err)
	}
	var output bytes.Buffer
	if err := WriteStreamRecord(&output, StreamPrimaryToReplica, ack); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("reverse ACK write error=%v", err)
	}
}

func TestStreamReadsAndWritesAcknowledgement(t *testing.T) {
	ack, err := (Acknowledgement{
		ClusterID: "cluster", Incarnation: "incarnation", NodeID: "replica",
		Index: 3, Term: 2, DurableIndex: 3, AppliedIndex: 2,
	}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := WriteStreamRecord(&output, StreamReplicaToPrimary, ack); err != nil {
		t.Fatal(err)
	}
	record, err := ReadStreamRecord(&output, StreamReplicaToPrimary)
	if err != nil || record.Kind != StreamRecordAcknowledgement || !bytes.Equal(record.Encoded, ack) {
		t.Fatalf("ack record kind=%d err=%v", record.Kind, err)
	}
}

func TestStreamRejectsUnknownOrTruncatedTypeWithoutAllocatingItsBody(t *testing.T) {
	unknown := make([]byte, streamPrefixBytes)
	binary.BigEndian.PutUint32(unknown[:4], 1<<30)
	copy(unknown[4:], []byte("UNKNOWN!"))
	if _, err := ReadStreamRecord(bytes.NewReader(unknown), StreamPrimaryToReplica); err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Fatalf("unknown record error=%v", err)
	}
	short := []byte{0, 0, 0, 7}
	if _, err := ReadStreamRecord(bytes.NewReader(short), StreamPrimaryToReplica); err == nil || !strings.Contains(err.Error(), "too short") {
		t.Fatalf("short record error=%v", err)
	}
}
