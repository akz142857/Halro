package semantic

import "testing"

// The tiers are a partition of InputTokens, and that is the whole reason the
// convention is safe to price against. An adapter that reported them as
// additions instead would make the cached span chargeable twice, so the invariant
// is enforced rather than documented.
func TestUsageRejectsCacheTiersThatOverrunTheInputTotal(t *testing.T) {
	usage := Usage{
		InputTokens:           1_000,
		CachedInputTokens:     900,
		CacheWriteInputTokens: 200, // 1100 > 1000
		OutputTokens:          10,
		TotalTokens:           1_010,
		Source:                UsageProviderReported,
	}
	if err := usage.Validate(); err == nil {
		t.Fatal("cache tiers summing past the input total were accepted")
	}
}

func TestUsageRejectsNegativeCacheTiers(t *testing.T) {
	for name, usage := range map[string]Usage{
		"cache read":  {InputTokens: 10, CachedInputTokens: -1, TotalTokens: 10, Source: UsageProviderReported},
		"cache write": {InputTokens: 10, CacheWriteInputTokens: -1, TotalTokens: 10, Source: UsageProviderReported},
	} {
		if err := usage.Validate(); err == nil {
			t.Fatalf("negative %s tier was accepted", name)
		}
	}
}

func TestUsageAcceptsTiersThatExactlyPartitionTheInput(t *testing.T) {
	usage := Usage{
		InputTokens:           1_000,
		CachedInputTokens:     800,
		CacheWriteInputTokens: 200,
		OutputTokens:          10,
		TotalTokens:           1_010,
		Source:                UsageProviderReported,
	}
	if err := usage.Validate(); err != nil {
		t.Fatalf("a fully partitioned prompt was rejected: %v", err)
	}
	if got := usage.UncachedInputTokens(); got != 0 {
		t.Fatalf("uncached remainder = %d, want 0", got)
	}
}

// Usage recorded before the tiers existed decodes with them zero, which must
// keep reading as "no cache reported" rather than failing validation.
func TestUsageWithoutTiersRemainsValid(t *testing.T) {
	usage := Usage{InputTokens: 500, OutputTokens: 60, TotalTokens: 560, Source: UsageProviderReported}
	if err := usage.Validate(); err != nil {
		t.Fatalf("tier-free usage was rejected: %v", err)
	}
	if got := usage.UncachedInputTokens(); got != 500 {
		t.Fatalf("uncached remainder = %d, want 500", got)
	}
}
