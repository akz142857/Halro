package sse

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestDecodeMultilineCommentsAndEncode(t *testing.T) {
	decoder := NewDecoder(strings.NewReader(": keepalive\nevent: message\ndata: one\ndata: two\n\n"), 1024)
	event, err := decoder.Next()
	if err != nil {
		t.Fatal(err)
	}
	if event.Event != "message" || string(event.Data) != "one\ntwo" {
		t.Fatalf("unexpected event: %#v", event)
	}
	var encoded bytes.Buffer
	if err := NewEncoder(&encoded).Write(event); err != nil {
		t.Fatal(err)
	}
	if encoded.String() != "event: message\ndata: one\ndata: two\n\n" {
		t.Fatalf("unexpected encoding: %q", encoded.String())
	}
}

func TestDecoderRejectsOversizedEvent(t *testing.T) {
	decoder := NewDecoder(strings.NewReader("data: "+strings.Repeat("x", 20)+"\n\n"), 10)
	if _, err := decoder.Next(); err == nil {
		t.Fatal("expected size rejection")
	}
}

func TestDecoderReturnsEOFAfterEvents(t *testing.T) {
	decoder := NewDecoder(strings.NewReader("data: x\n\n"), 1024)
	if _, err := decoder.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := decoder.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
}
