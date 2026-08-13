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

// Tool families are matched by family, not by exact version, so an upstream
// version bump must not need a code change here. The dated types below are the
// ones Anthropic ships today; the far-future one stands in for the next bump.
func TestDecodeMessageRequestClassifiesToolsByExecutionSite(t *testing.T) {
	body := func(tool string) string {
		return `{"model":"m","max_tokens":10,"tools":[` + tool + `],"messages":[{"role":"user","content":"hi"}]}`
	}
	for _, testCase := range []struct {
		name    string
		tool    string
		wantErr string
	}{
		{"bash", `{"type":"bash_20250124","name":"bash"}`, ""},
		{"text editor", `{"type":"text_editor_20250728","name":"str_replace_based_edit_tool"}`, ""},
		{"memory", `{"type":"memory_20250818","name":"memory"}`, ""},
		{"computer keeps display fields", `{"type":"computer_20251124","name":"computer","display_width_px":1024,"display_height_px":768}`, ""},
		{"unreleased version of a known family", `{"type":"text_editor_29991231","name":"editor"}`, ""},
		{"custom", `{"name":"lookup","input_schema":{"type":"object"}}`, ""},
		{"custom keeps cache_control", `{"name":"lookup","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}`, ""},
		{"client tool rejects a schema", `{"type":"bash_20250124","name":"bash","input_schema":{"type":"object"}}`, "no input schema"},
		{"web search", `{"type":"web_search_20260209","name":"web_search"}`, "provider-executed"},
		{"code execution", `{"type":"code_execution_20260521","name":"code_execution"}`, "provider-executed"},
		{"unknown family", `{"type":"frobnicate_20260101","name":"frobnicate"}`, "unrecognised tool type"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := DecodeMessageRequest(strings.NewReader(body(testCase.tool)))
			switch {
			case testCase.wantErr == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case testCase.wantErr != "" && (err == nil || !strings.Contains(err.Error(), testCase.wantErr)):
				t.Fatalf("want error containing %q, got %v", testCase.wantErr, err)
			}
		})
	}
}

// A tool declaration must reach the provider byte-identical. Anything the
// struct does not model — cache_control, defer_loading, the computer tool's
// display dimensions — rides through Raw rather than being dropped.
func TestToolRoundTripPreservesUnmodelledFields(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"tools":[{"type":"computer_20251124","name":"computer","display_width_px":1024,"cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`
	request, err := DecodeMessageRequest(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(request.Tools)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"display_width_px":1024`, `"cache_control":{"type":"ephemeral"}`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("tool lost %s: %s", want, encoded)
		}
	}
}

// document and search_result carry input data the same way image does, so they
// belong on the accepted side of a boundary drawn by execution site.
func TestDecodeMessageRequestAcceptsDataCarryingBlocks(t *testing.T) {
	for _, testCase := range []struct{ name, block string }{
		{"document", `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AA=="}}`},
		{"search_result", `{"type":"search_result","title":"t","source":"s","content":[{"type":"text","text":"x"}]}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[` + testCase.block + `]}]}`
			if _, err := DecodeMessageRequest(strings.NewReader(body)); err != nil {
				t.Fatalf("want accepted, got %v", err)
			}
		})
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
