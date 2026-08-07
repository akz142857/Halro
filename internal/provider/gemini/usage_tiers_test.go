package gemini

import "testing"

// Gemini splits its counters the other way from Anthropic: promptTokenCount
// already includes the cached span, while thoughtsTokenCount sits *outside*
// candidatesTokenCount even though it bills at the output rate. Reading only
// promptTokenCount and candidatesTokenCount therefore prices the prompt right
// and drops the thinking span entirely.
func TestGeminiUsageFoldsThinkingIntoOutputAndKeepsCacheASubset(t *testing.T) {
	usage := openAIUsage(usageMetadata{
		PromptTokenCount:        20_000,
		CachedContentTokenCount: 15_000,
		CandidatesTokenCount:    400,
		ThoughtsTokenCount:      1_100,
		TotalTokenCount:         21_500,
	})
	if usage == nil {
		t.Fatal("usage was discarded")
	}
	// The cached span is already inside promptTokenCount, so the prompt total
	// must not grow — adding it would double-count.
	if usage.PromptTokens != 20_000 {
		t.Fatalf("prompt tokens = %d, want 20000", usage.PromptTokens)
	}
	if got := usage.CachedPromptTokens(); got != 15_000 {
		t.Fatalf("cached tier = %d, want 15000", got)
	}
	// Thinking bills at the output rate and is not in candidatesTokenCount, so
	// it has to be folded in or it is never charged.
	if usage.CompletionTokens != 1_500 {
		t.Fatalf("completion tokens = %d, want 1500 (400 visible + 1100 thinking)", usage.CompletionTokens)
	}
	if got := usage.ReasoningTokens(); got != 1_100 {
		t.Fatalf("reasoning tokens = %d, want 1100", got)
	}
	if usage.TotalTokens != 21_500 {
		t.Fatalf("total = %d, want 21500", usage.TotalTokens)
	}
}

func TestGeminiUsageWithoutCacheOrThinkingIsUnchanged(t *testing.T) {
	usage := openAIUsage(usageMetadata{
		PromptTokenCount: 900, CandidatesTokenCount: 120, TotalTokenCount: 1_020,
	})
	if usage == nil {
		t.Fatal("usage was discarded")
	}
	if usage.PromptTokens != 900 || usage.CompletionTokens != 120 || usage.TotalTokens != 1_020 {
		t.Fatalf("plain usage changed: %#v", usage)
	}
	if usage.PromptTokensDetails != nil || usage.CompletionTokensDetails != nil {
		t.Fatalf("empty tiers should stay off the wire: %#v", usage)
	}
}
