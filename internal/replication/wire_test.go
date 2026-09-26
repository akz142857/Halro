package replication

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestLengthDelimitedWireBoundsBeforeAllocation(t *testing.T) {
	hello, err := testHello("halro-0", RolePrimary, 1).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	if err := WriteLengthDelimited(&stream, hello, MaxHelloBytes); err != nil {
		t.Fatal(err)
	}
	decoded, err := ReadLengthDelimited(&stream, MaxHelloBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, hello) {
		t.Fatal("wire record changed")
	}

	var excessive [4]byte
	binary.BigEndian.PutUint32(excessive[:], MaxHelloBytes+1)
	if _, err := ReadLengthDelimited(bytes.NewReader(excessive[:]), MaxHelloBytes); err == nil || !strings.Contains(err.Error(), "bound") {
		t.Fatalf("excessive length error=%v", err)
	}
	if _, err := ReadLengthDelimited(bytes.NewReader(hello[:len(hello)-1]), MaxHelloBytes); err == nil || !strings.Contains(err.Error(), "body") {
		t.Fatalf("partial body error=%v", err)
	}
}
