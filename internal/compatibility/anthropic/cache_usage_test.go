package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestCacheWriteUsageRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name, detail string
		long         int64
		exact        bool
	}{
		{"5m", `,"cache_creation":{"ephemeral_5m_input_tokens":8}`, 0, true},
		{"1h", `,"cache_creation":{"ephemeral_1h_input_tokens":8}`, 8, true},
		{"mixed", `,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":3}`, 3, true},
		{"total only", "", 0, false},
		{"conflicting", `,"cache_creation":{"ephemeral_1h_input_tokens":12}`, 8, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire anthropicapi.Usage
			if err := json.Unmarshal([]byte(`{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":4,"cache_creation_input_tokens":8`+tc.detail+"}"), &wire); err != nil {
				t.Fatal(err)
			}
			usage := DecodeUsage(wire)
			if err := usage.Validate(); err != nil || usage.InputTokens != 22 || usage.CacheWriteInputTokens != 8 || usage.CacheWriteLongInputTokens != tc.long {
				t.Fatalf("usage=%+v err=%v", usage, err)
			}
			rendered := renderUsage(usage)
			if rendered.InputTokens != 10 || rendered.CacheReadInputTokens != 4 || rendered.CacheCreationInputTokens != 8 {
				t.Fatalf("rendered usage=%+v", rendered)
			}
			if detail := rendered.CacheCreation; tc.exact != (detail != nil) ||
				(detail != nil && (detail.Ephemeral5mInputTokens != 8-tc.long || detail.Ephemeral1hInputTokens != tc.long)) {
				t.Fatalf("rendered detail=%+v", detail)
			}
			usage.CacheWriteTTLPair = semantic.CacheWriteTTL30m12h
			if rendered := renderUsage(usage); rendered.CacheCreation != nil || rendered.CacheCreationInputTokens != 8 {
				t.Fatalf("Responses lifetimes were rendered as Messages lifetimes: %+v", rendered)
			}
		})
	}
}
