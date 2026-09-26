package replication

import (
	"bytes"
	"testing"
)

func TestClusterKeyIsBoundToTheIncarnation(t *testing.T) {
	master := bytes.Repeat([]byte{0x42}, 32)
	first, err := DeriveClusterKey(master, "inc_01")
	if err != nil {
		t.Fatal(err)
	}
	again, err := DeriveClusterKey(master, "inc_01")
	if err != nil {
		t.Fatal(err)
	}
	other, err := DeriveClusterKey(master, "inc_02")
	if err != nil {
		t.Fatal(err)
	}
	if first != again || first == other {
		t.Fatalf("same incarnation stable=%t other separated=%t", first == again, first != other)
	}
	if _, err := DeriveClusterKey(master[:31], "inc_01"); err == nil {
		t.Fatal("short Master Key was accepted")
	}
}
