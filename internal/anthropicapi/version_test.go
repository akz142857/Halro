package anthropicapi

import (
	"encoding/json"
	"testing"
)

func TestVersionSpecificRemovals(t *testing.T) {
	for _, test := range []struct {
		name, body string
		rejected   bool
	}{
		{"plain", `{"messages":[{"role":"user","content":"hi"}]}`, false},
		{"default sampling", `{"temperature":1,"messages":[]}`, true},
		{"null sampling", `{"top_p":null,"messages":[]}`, true},
		{"top k", `{"top_k":1,"messages":[]}`, true},
		{"assistant document", `{"messages":[{"role":"assistant","content":[{"type":"document"}]}]}`, true},
		{"nested assistant document", `{"messages":[{"role":"assistant","content":[{"type":"server_tool_result","content":[{"type":"document"}]}]}]}`, false},
		{"user citation", `{"messages":[{"role":"user","content":[{"type":"text","text":"x","citations":[{}]}]}]}`, true},
		{"search citation", `{"messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"search_result","content":[{"type":"text","text":"x","citations":[{}]}]}]}]}]}`, true},
		{"document setting", `{"messages":[{"role":"user","content":[{"type":"document","citations":{"enabled":true}}]}]}`, false},
		{"assistant citation", `{"messages":[{"role":"assistant","content":[{"type":"text","text":"x","citations":[{}]}]}]}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateVersionPayload(CurrentVersion, json.RawMessage(test.body)); (err != nil) != test.rejected {
				t.Fatalf("current version error=%v, rejected=%v", err, test.rejected)
			}
			if err := ValidateVersionPayload(SupportedVersion, json.RawMessage(test.body)); err != nil {
				t.Fatalf("legacy version changed: %v", err)
			}
		})
	}
	if err := ValidateVersionPayload("2099-01-01", json.RawMessage(`{}`)); err == nil {
		t.Fatal("unknown version accepted")
	}
}
