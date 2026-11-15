package openai

import (
	"encoding/json"
	"testing"

	"github.com/akz142857/Halro/internal/openaiapi"
)

func TestResponseCacheWriteUsageRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name, detail string
		long         int64
	}{
		{"30m", `,"cache_write_tokens_details":{"ttl_30m_tokens":8}`, 0},
		{"12h", `,"cache_write_tokens_details":{"ttl_12h_tokens":8}`, 8},
		{"mixed", `,"cache_write_tokens_details":{"ttl_30m_tokens":5,"ttl_12h_tokens":3}`, 3},
		{"total only", "", 0},
		{"conflicting", `,"cache_write_tokens_details":{"ttl_12h_tokens":12}`, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire openaiapi.ResponseUsage
			if err := json.Unmarshal([]byte(`{"input_tokens":22,"output_tokens":2,"total_tokens":24,"input_tokens_details":{"cached_tokens":4,"cache_write_tokens":8`+tc.detail+"}}"), &wire); err != nil {
				t.Fatal(err)
			}
			usage := DecodeResponseUsage(&wire)
			if err := usage.Validate(); err != nil || usage.CacheWriteInputTokens != 8 || usage.CacheWriteLongInputTokens != tc.long {
				t.Fatalf("usage=%+v err=%v", usage, err)
			}
			rendered := renderResponseUsage(usage).InputTokensDetails
			if rendered.CacheWriteTokens != 8 || rendered.CacheWriteTokensDetails == nil ||
				rendered.CacheWriteTokensDetails.TTL30mTokens != 8-tc.long || rendered.CacheWriteTokensDetails.TTL12hTokens != tc.long {
				t.Fatalf("rendered usage=%+v", rendered)
			}
		})
	}
}
