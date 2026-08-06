package sse

import (
	"bytes"
	"strings"
	"testing"
)

// FuzzEncodeDecodeRoundTrip pins the one property the codec owes its callers:
// whatever a payload contains, it comes back out as a payload and never as
// structure. The wire format has no escape, so the encoder folds every line
// terminator into its own data: line — which means the round trip is exact once
// CR is normalised to LF, and an injected "data:" or blank line stays text.
func FuzzEncodeDecodeRoundTrip(f *testing.F) {
	for _, seed := range []struct{ event, data string }{
		{"message", "hello"},
		{"message", "one\ntwo"},
		{"", "carriage\rreturn"},
		{"message", "crlf\r\nline"},
		{"message", "data: injected"},
		{"message", "blank\n\nline"},
		{"message", ": comment"},
		{"message", "高度机密\r\n内容"},
		{"", ""},
	} {
		f.Add(seed.event, seed.data)
	}
	f.Fuzz(func(t *testing.T, event, data string) {
		// The event name occupies one line of the frame; a terminator inside it
		// is a caller error the codec does not claim to survive.
		if strings.ContainsAny(event, "\r\n") || len(event)+len(data) > 16<<10 {
			t.Skip()
		}
		var encoded bytes.Buffer
		if err := NewEncoder(&encoded).Write(Event{Event: event, Data: []byte(data)}); err != nil {
			t.Fatalf("encode: %v", err)
		}
		if strings.Contains(encoded.String(), "\r") {
			t.Fatalf("encoder emitted a raw CR, which a client reads as a line boundary: %q", encoded.String())
		}
		decoded, err := NewDecoder(strings.NewReader(encoded.String()), 1<<20).Next()
		if err != nil {
			t.Fatalf("decode %q: %v", encoded.String(), err)
		}
		if decoded.Event != event {
			t.Fatalf("event name changed: %q -> %q", event, decoded.Event)
		}
		expected := strings.ReplaceAll(strings.ReplaceAll(data, "\r\n", "\n"), "\r", "\n")
		if string(decoded.Data) != expected {
			t.Fatalf("payload changed shape: %q -> %q", expected, decoded.Data)
		}
	})
}

// FuzzDecoderNeverPanics feeds arbitrary bytes to the decoder. Every input is
// either an event or an error; a gateway parsing an upstream's stream cannot
// afford a third outcome.
func FuzzDecoderNeverPanics(f *testing.F) {
	for _, seed := range []string{
		"event: message\ndata: one\n\n",
		"data: one\rdata: two\r\n\n",
		": comment only\n\n",
		"data:\n\n",
		"id: 1\nevent: x\ndata: y\n\n",
		"\r\n\r\n",
		"data: " + strings.Repeat("x", 4096),
		"event message no colon\n\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 16<<10 {
			t.Skip()
		}
		decoder := NewDecoder(strings.NewReader(raw), 8<<10)
		for events := 0; events < 64; events++ {
			if _, err := decoder.Next(); err != nil {
				return
			}
		}
	})
}
