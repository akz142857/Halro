package replication

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestCommitNoticeVersionOneGoldenEncoding(t *testing.T) {
	notice := CommitNotice{ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-0", Term: 7, ConfirmedIndex: 11}
	encoded, err := notice.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	const wantHex = "00000045484c5243464d3031000100000000000000000007000000000000000b000c000600070000000000000000000070726f64756374696f6e2d61696e635f303168616c726f2d30"
	if got := hex.EncodeToString(encoded); got != wantHex {
		t.Fatalf("version-1 commit notice fixture changed\n got: %s\nwant: %s", got, wantHex)
	}
	decoded, err := UnmarshalCommitNotice(encoded)
	if err != nil || decoded != notice {
		t.Fatalf("decoded=%#v err=%v", decoded, err)
	}
}

func TestCommitNoticeRefusesTrailingBytesAndReservedFields(t *testing.T) {
	encoded, err := (CommitNotice{ClusterID: "c", Incarnation: "i", NodeID: "n", Term: 1, ConfirmedIndex: 1}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalCommitNotice(append(encoded, 0)); err == nil || !strings.Contains(err.Error(), "length") {
		t.Fatalf("trailing error=%v", err)
	}
	encoded[4+34] = 1
	if _, err := UnmarshalCommitNotice(encoded); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved error=%v", err)
	}
}
