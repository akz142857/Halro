package openai

import (
	"encoding/json"
	"testing"

	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestResponseCacheWriteUsageRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name, detail string
		long         int64
		exact        bool
	}{
		{"30m", `,"cache_write_tokens_details":{"ttl_30m_tokens":8}`, 0, true},
		{"12h", `,"cache_write_tokens_details":{"ttl_12h_tokens":8}`, 8, true},
		{"mixed", `,"cache_write_tokens_details":{"ttl_30m_tokens":5,"ttl_12h_tokens":3}`, 3, true},
		{"total only", "", 0, false},
		{"conflicting", `,"cache_write_tokens_details":{"ttl_12h_tokens":12}`, 8, false},
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
			rendered := renderResponseUsage(decodeUsage(renderUsage(usage))).InputTokensDetails
			if detail := rendered.CacheWriteTokensDetails; rendered.CacheWriteTokens != 8 || tc.exact != (detail != nil) ||
				(detail != nil && (detail.TTL30mTokens != 8-tc.long || detail.TTL12hTokens != tc.long)) {
				t.Fatalf("rendered usage=%+v", rendered)
			}
			usage.CacheWriteTTLPair = semantic.CacheWriteTTL5m1h
			if rendered := renderResponseUsage(usage).InputTokensDetails; rendered.CacheWriteTokensDetails != nil || rendered.CacheWriteTokens != 8 {
				t.Fatalf("Messages lifetimes were rendered as Responses lifetimes: %+v", rendered)
			}
		})
	}
}
