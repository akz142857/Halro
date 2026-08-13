package anthropicapi

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeMessageRequestPreservesThinkingBlocks(t *testing.T) {
	payload := []byte(`{"model":"claude","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"private","signature":"sig-value"},{"type":"redacted_thinking","data":"opaque"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}]}`)
	request, err := DecodeMessageRequest(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(request.Raw, payload) {
		t.Fatal("raw payload changed")
	}
	encoded, err := json.Marshal(request.Messages[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"signature":"sig-value"`, `"data":"opaque"`, `"type":"tool_use"`} {
		if !bytes.Contains(encoded, []byte(expected)) {
			t.Fatalf("missing %s in %s", expected, encoded)
		}
	}
}

func TestDecodeMessageRequestRejectsUnknownTopLevelField(t *testing.T) {
	_, err := DecodeMessageRequest(bytes.NewBufferString(`{"model":"claude","max_tokens":1,"messages":[{"role":"user","content":"hi"}],"cache_control":{}}`))
	if err == nil {
		t.Fatal("expected unknown field rejection")
	}
}

func TestParseExecutionMode(t *testing.T) {
	for value, expected := range map[string]ExecutionMode{"": ModePortable, "portable": ModePortable, "NATIVE": ModeNative} {
		actual, err := ParseExecutionMode(value)
		if err != nil || actual != expected {
			t.Fatalf("%q: %v %q", value, err, actual)
		}
	}
	if _, err := ParseExecutionMode("guess"); err == nil {
		t.Fatal("expected invalid mode")
	}
}

func TestEnabledThinkingRejectsForcedToolChoice(t *testing.T) {
	for _, choice := range []string{`{"type":"any"}`, `{"type":"tool","name":"lookup"}`} {
		payload := `{"model":"claude","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"thinking":{"type":"enabled","budget_tokens":32},"tool_choice":` + choice + `}`
		if _, err := DecodeMessageRequest(bytes.NewBufferString(payload)); err == nil {
			t.Fatalf("expected forced tool choice %s to be rejected with enabled thinking", choice)
		}
	}
	if _, err := DecodeMessageRequest(bytes.NewBufferString(`{"model":"claude","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":32},"tool_choice":{"type":"auto"}}`)); err != nil {
		t.Fatalf("auto tool choice should remain valid: %v", err)
	}
}

// A duplicate object member makes the document Halro inspects and the document
// the provider receives two different things: encoding/json resolves duplicates
// last-wins, while the native path forwards the caller's original bytes. That
// gap is a redaction bypass — a Gateway Key in the losing copy is invisible to
// inspection and still on the wire — so the ambiguity is refused outright.
func TestDecodeMessageRequestRejectsDuplicateMembers(t *testing.T) {
	gatewayKey := "gw_" + strings.Repeat("A", 44)
	for _, testCase := range []struct{ name, body string }{
		{"secret hidden behind a duplicate", `{"model":"m","max_tokens":10,"metadata":{"user_id":"` + gatewayKey + `","user_id":"benign"},"messages":[{"role":"user","content":"hi"}]}`},
		{"tool type declared twice", `{"model":"m","max_tokens":10,"tools":[{"type":"code_execution_20250825","name":"x","input_schema":{"type":"object"},"type":"custom"}],"messages":[{"role":"user","content":"hi"}]}`},
		{"top level", `{"model":"a","model":"b","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`},
		{"inside a content block", `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"a","text":"b"}]}]}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := DecodeMessageRequest(strings.NewReader(testCase.body)); err == nil {
				t.Fatal("duplicate member accepted")
			}
		})
	}
	// Control: the same shapes without a duplicate still decode.
	body := `{"model":"m","max_tokens":10,"metadata":{"user_id":"benign"},"tools":[{"type":"custom","name":"x","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":[{"type":"text","text":"a"}]}]}`
	if _, err := DecodeMessageRequest(strings.NewReader(body)); err != nil {
		t.Fatalf("control rejected: %v", err)
	}
}
