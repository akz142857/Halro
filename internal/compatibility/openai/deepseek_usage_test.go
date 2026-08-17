package openai

import (
	"encoding/json"
	"testing"

	"github.com/akz142857/Halro/internal/openaiapi"
)

// DeepSeek speaks this wire format but reports the cache-read split as two
// sibling counters on usage rather than inside prompt_tokens_details. A decoder
// that only knows OpenAI's spelling reads every hit as zero, so the hit span is
// settled at the miss rate — thirty times the price of what it was, on the
// rate card DeepSeek publishes for both v4 models.
func TestDeepSeekPromptCacheCountersDecodeIntoTheCachedTier(t *testing.T) {
	var wire openaiapi.Usage
	if err := json.Unmarshal([]byte(`{
		"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15,
		"prompt_cache_hit_tokens": 8, "prompt_cache_miss_tokens": 2,
		"completion_tokens_details": {"reasoning_tokens": 3}
	}`), &wire); err != nil {
		t.Fatal(err)
	}
	usage := decodeUsage(&wire)
	if usage == nil {
		t.Fatal("usage was dropped")
	}
	if usage.InputTokens != 10 || usage.CachedInputTokens != 8 || usage.UncachedInputTokens() != 2 {
		t.Fatalf("cache tiers were not decoded: %#v", usage)
	}
	if usage.OutputTokens != 5 || usage.ReasoningTokens != 3 || usage.TotalTokens != 15 {
		t.Fatalf("unexpected usage: %#v", usage)
	}
	if err := usage.Validate(); err != nil {
		t.Fatalf("decoded usage is not valid: %v", err)
	}
}

// The two spellings must never both count. OpenAI's details object is the one
// this struct also renders, so it wins; DeepSeek's counter is a fallback rather
// than a second tier to add on.
func TestOpenAICachedTokensWinOverTheDeepSeekCounter(t *testing.T) {
	wire := openaiapi.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, PromptCacheHitTokens: 8}
	wire.SetCachedPromptTokens(4)
	usage := decodeUsage(&wire)
	if usage.CachedInputTokens != 4 {
		t.Fatalf("cached tier = %d, want the details object's 4", usage.CachedInputTokens)
	}
}

// The documented invariant is prompt_tokens = hit + miss, which is already the
// subset convention semantic.Usage requires. The other reading of the same two
// numbers — a prompt_tokens counting only the misses, the way Anthropic reports
// input_tokens — would price the hit span twice at zero: once missing from the
// input total and once excluded from it as cached. It is detectable because it
// is the only reading where the partition overruns its own prompt.
func TestAPromptTokenCountThatExcludesCacheHitsIsRecovered(t *testing.T) {
	wire := openaiapi.Usage{PromptTokens: 2, CompletionTokens: 5, TotalTokens: 7, PromptCacheHitTokens: 8, PromptCacheMissTokens: 2}
	usage := decodeUsage(&wire)
	if usage.InputTokens != 10 || usage.CachedInputTokens != 8 || usage.UncachedInputTokens() != 2 {
		t.Fatalf("prompt span was not recovered: %#v", usage)
	}
	if usage.TotalTokens != 15 {
		t.Fatalf("total = %d, want the recovered prompt plus completion", usage.TotalTokens)
	}
	if err := usage.Validate(); err != nil {
		t.Fatalf("recovered usage is not valid: %v", err)
	}
}

// A cache tier larger than the prompt it partitions fails semantic validation,
// which costs the attempt its provider-reported usage entirely and silently
// downgrades it to an estimate. Clamping settles the disputed span at the
// ordinary input rate instead — the more expensive of the two readings.
func TestACacheTierLargerThanThePromptIsClampedRatherThanDropped(t *testing.T) {
	wire := openaiapi.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, CacheWriteTokens: 4}
	wire.SetCachedPromptTokens(9)
	usage := decodeUsage(&wire)
	if usage.CachedInputTokens != 6 || usage.CacheWriteInputTokens != 4 || usage.UncachedInputTokens() != 0 {
		t.Fatalf("tiers were not clamped onto the prompt: %#v", usage)
	}
	if err := usage.Validate(); err != nil {
		t.Fatalf("clamped usage is not valid: %v", err)
	}
}
