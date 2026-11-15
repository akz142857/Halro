package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/akz142857/Halro/internal/anthropicapi"
)

func TestCacheWriteUsageRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name, detail string
		long         int64
	}{
		{"5m", `,"cache_creation":{"ephemeral_5m_input_tokens":8}`, 0},
		{"1h", `,"cache_creation":{"ephemeral_1h_input_tokens":8}`, 8},
		{"mixed", `,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":3}`, 3},
		{"total only", "", 0},
		{"conflicting", `,"cache_creation":{"ephemeral_1h_input_tokens":12}`, 8},
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
			if rendered.InputTokens != 10 || rendered.CacheReadInputTokens != 4 || rendered.CacheCreationInputTokens != 8 ||
				rendered.CacheCreation == nil || rendered.CacheCreation.Ephemeral5mInputTokens != 8-tc.long || rendered.CacheCreation.Ephemeral1hInputTokens != tc.long {
				t.Fatalf("rendered usage=%+v", rendered)
			}
		})
	}
}
