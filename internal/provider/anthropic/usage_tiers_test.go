package anthropic

import (
	"testing"

	"github.com/akz142857/Heimdall/internal/anthropicapi"
)

// Anthropic reports input_tokens net of both cache tiers. Reading that field as
// the prompt span is what made a cache-served request look ~20x cheaper than it
// was: on an agent workload the cache serves most of the prompt, so the part
// Heimdall recorded was the small uncached remainder.
func TestAnthropicUsageRecoversThePromptSpanTheCacheServed(t *testing.T) {
	// A long agent turn: 100k prompt, almost all of it served from cache.
	wire := anthropicapi.Usage{
		InputTokens:              5_000,
		CacheReadInputTokens:     90_000,
		CacheCreationInputTokens: 5_000,
		OutputTokens:             800,
		ThinkingTokens:           300,
	}

	if got := wire.PromptTokens(); got != 100_000 {
		t.Fatalf("prompt span = %d, want 100000", got)
	}

	portable := portableUsage(wire)
	if portable.PromptTokens != 100_000 {
		t.Fatalf("portable prompt tokens = %d, want 100000 (the pre-fix value was 5000)", portable.PromptTokens)
	}
	if got := portable.CachedPromptTokens(); got != 90_000 {
		t.Fatalf("cache-read tier = %d, want 90000", got)
	}
	if portable.CacheWriteTokens != 5_000 {
		t.Fatalf("cache-write tier = %d, want 5000", portable.CacheWriteTokens)
	}
	if got := portable.ReasoningTokens(); got != 300 {
		t.Fatalf("reasoning tokens = %d, want 300", got)
	}
	if portable.TotalTokens != 100_800 {
		t.Fatalf("total = %d, want 100800", portable.TotalTokens)
	}

	// The streaming bridge must agree with the buffered path; the two used to
	// disagree, and only one of them carried thinking tokens at all.
	streamed := semanticUsage(wire)
	if err := streamed.Validate(); err != nil {
		t.Fatalf("streamed usage failed validation: %v", err)
	}
	if streamed.InputTokens != portable.PromptTokens ||
		streamed.CachedInputTokens != portable.CachedPromptTokens() ||
		streamed.CacheWriteInputTokens != portable.CacheWriteTokens ||
		streamed.ReasoningTokens != portable.ReasoningTokens() {
		t.Fatalf("streaming and buffered paths disagree: %#v vs %#v", streamed, portable)
	}
	// The tiers partition the prompt; the uncached remainder is what a plain
	// input rate legitimately applies to.
	if got := streamed.UncachedInputTokens(); got != 5_000 {
		t.Fatalf("uncached remainder = %d, want 5000", got)
	}
}

// An uncached request must be unaffected, or the fix would move every existing
// number rather than just the cached ones.
func TestAnthropicUsageWithoutCacheIsUnchanged(t *testing.T) {
	wire := anthropicapi.Usage{InputTokens: 1_200, OutputTokens: 340}
	portable := portableUsage(wire)
	if portable.PromptTokens != 1_200 || portable.CompletionTokens != 340 || portable.TotalTokens != 1_540 {
		t.Fatalf("uncached usage changed: %#v", portable)
	}
	if portable.PromptTokensDetails != nil || portable.CompletionTokensDetails != nil {
		t.Fatalf("empty tiers should stay off the wire: %#v", portable)
	}
}
