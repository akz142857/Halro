package bedrock

import "testing"

// Converse reports the cache tiers alongside inputTokens, and the struct did not
// even parse them, so a cache-served Bedrock request was invisible twice over:
// the tokens were absent from the ledger and the cost was computed from the
// uncached remainder alone.
func TestBedrockConverseUsageCountsBothCacheTiers(t *testing.T) {
	usage, err := openAIUsage(tokenUsage{
		InputTokens:           2_000,
		CacheReadInputTokens:  40_000,
		CacheWriteInputTokens: 8_000,
		OutputTokens:          500,
		TotalTokens:           50_500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if usage.PromptTokens != 50_000 {
		t.Fatalf("prompt tokens = %d, want 50000 (the pre-fix value was 2000)", usage.PromptTokens)
	}
	if got := usage.CachedPromptTokens(); got != 40_000 {
		t.Fatalf("cache-read tier = %d, want 40000", got)
	}
	if usage.CacheWriteTokens != 8_000 {
		t.Fatalf("cache-write tier = %d, want 8000", usage.CacheWriteTokens)
	}
	if usage.TotalTokens != 50_500 {
		t.Fatalf("total = %d, want 50500", usage.TotalTokens)
	}
}

// A request served entirely from cache reports zero uncached input. The old
// emptiness check keyed on inputTokens alone, so such a request was treated as
// "no usage reported" and silently downgraded to a local estimate.
func TestBedrockFullyCachedUsageIsNotMistakenForMissingUsage(t *testing.T) {
	usage, err := openAIUsage(tokenUsage{
		InputTokens:          0,
		CacheReadInputTokens: 30_000,
		OutputTokens:         0,
		TotalTokens:          0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if usage == nil {
		t.Fatal("fully cached usage was discarded as empty")
	}
	if usage.PromptTokens != 30_000 {
		t.Fatalf("prompt tokens = %d, want 30000", usage.PromptTokens)
	}
}

func TestBedrockUsageRejectsNegativeCacheTiers(t *testing.T) {
	if _, err := openAIUsage(tokenUsage{InputTokens: 10, CacheReadInputTokens: -1}); err == nil {
		t.Fatal("negative cache tier was accepted")
	}
}

// Genuinely absent usage must still read as absent.
func TestBedrockEmptyUsageStaysEmpty(t *testing.T) {
	usage, err := openAIUsage(tokenUsage{})
	if err != nil {
		t.Fatal(err)
	}
	if usage != nil {
		t.Fatalf("empty usage produced %#v", usage)
	}
}
