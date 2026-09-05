package sse

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
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

func TestBareCarriageReturnEventIsDeliveredBeforeEOF(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	decoder := NewDecoder(reader, 1024)
	done := make(chan Event, 1)
	errs := make(chan error, 1)
	go func() {
		event, err := decoder.Next()
		if err != nil {
			errs <- err
			return
		}
		done <- event
	}()
	if _, err := io.WriteString(writer, "data: one\r\r"); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-done:
		if string(event.Data) != "one" {
			t.Fatalf("event = %#v", event)
		}
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("complete CR-only event waited for EOF")
	}
	_ = writer.Close()
}

func TestCRLFMaySpanReadsWithoutCreatingAnEmptyEvent(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	decoder := NewDecoder(reader, 1024)
	go func() {
		_, _ = io.WriteString(writer, "event: message\r")
		_, _ = io.WriteString(writer, "\ndata: one\r")
		_, _ = io.WriteString(writer, "\n\r")
		_, _ = io.WriteString(writer, "\n")
		_ = writer.Close()
	}()
	event, err := decoder.Next()
	if err != nil {
		t.Fatal(err)
	}
	if event.Event != "message" || string(event.Data) != "one" {
		t.Fatalf("cross-read CRLF changed event: %#v", event)
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

// The SSE grammar ends a line on CR, LF or CRLF. bufio only knows LF, so a bare
// CR used to be delivered as part of the preceding line — and on the encoder
// side, emitted raw, where a client would read it as a field boundary the
// upstream never wrote.
func TestBareCarriageReturnIsALineTerminatorOnBothSides(t *testing.T) {
	decoder := NewDecoder(strings.NewReader("event: message\rdata: one\rdata: two\r\n\n"), 1024)
	event, err := decoder.Next()
	if err != nil {
		t.Fatal(err)
	}
	if event.Event != "message" || string(event.Data) != "one\ntwo" {
		t.Fatalf("bare CR was not treated as a line terminator: %#v", event)
	}

	// CRLF must still be one terminator, not two.
	decoder = NewDecoder(strings.NewReader("event: message\r\ndata: one\r\n\r\n"), 1024)
	if event, err = decoder.Next(); err != nil {
		t.Fatal(err)
	}
	if event.Event != "message" || string(event.Data) != "one" {
		t.Fatalf("CRLF was split into two terminators: %#v", event)
	}

	// A payload that carries a CR must not be able to close its own data line.
	var encoded bytes.Buffer
	if err := NewEncoder(&encoded).Write(Event{Event: "message", Data: []byte("safe\rdata: injected")}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded.String(), "\r") {
		t.Fatalf("encoder emitted a raw CR: %q", encoded.String())
	}
	if encoded.String() != "event: message\ndata: safe\ndata: data: injected\n\n" {
		t.Fatalf("unexpected encoding: %q", encoded.String())
	}
	// Round trip: the injected text stays inside the payload rather than
	// becoming a field of its own.
	roundTripped, err := NewDecoder(strings.NewReader(encoded.String()), 1024).Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(roundTripped.Data) != "safe\ndata: injected" {
		t.Fatalf("payload changed shape across a round trip: %q", roundTripped.Data)
	}
}
