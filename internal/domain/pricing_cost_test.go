package domain

import (
	"math"
	"testing"
)

func TestCalculateUSDTokensV1RoundsComponentsIndependently(t *testing.T) {
	price := validPriceVersion(t, "price_formula", 1, "2026-08-04T00:00:00Z")
	price.InputMicrosPerMillion = 400_000
	price.OutputMicrosPerMillion = 1_600_000
	price.FixedRequestMicrosUSD = 3
	cost, err := CalculateUSDTokensV1(1, 0, 1, price)
	if err != nil {
		t.Fatal(err)
	}
	if cost.InputCostMicrosUSD != 1 || cost.OutputCostMicrosUSD != 2 || cost.FixedCostMicrosUSD != 3 || cost.TotalCostMicrosUSD != 6 {
		t.Fatalf("cost=%#v", cost)
	}
}

func TestCalculateUSDTokensV1UsesWideIntermediateAndRejectsFinalOverflow(t *testing.T) {
	price := validPriceVersion(t, "price_wide", 1, "2026-08-04T00:00:00Z")
	price.InputMicrosPerMillion = math.MaxInt64
	price.OutputMicrosPerMillion = 0
	cost, err := CalculateUSDTokensV1(1_000_000, 0, 0, price)
	if err != nil || cost.TotalCostMicrosUSD != math.MaxInt64 {
		t.Fatalf("wide intermediate cost=%#v err=%v", cost, err)
	}
	if _, err := CalculateUSDTokensV1(1_000_001, 0, 0, price); err == nil {
		t.Fatal("expected final int64 overflow")
	}
}

func TestCalculateUSDTokensV1KeepsExplicitFreeKnown(t *testing.T) {
	price := validPriceVersion(t, "price_free_formula", 1, "2026-08-04T00:00:00Z")
	price.BillingMode = BillingModeFree
	price.InputMicrosPerMillion, price.CachedInputMicrosPerMillion, price.OutputMicrosPerMillion = 0, 0, 0
	cost, err := CalculateUSDTokensV1(math.MaxInt64, math.MaxInt64, math.MaxInt64, price)
	if err != nil || cost != (PriceCostBreakdown{}) {
		t.Fatalf("free cost=%#v err=%v", cost, err)
	}
}

// A cached prompt token is billed at the cache-read rate, and only the rest of
// the prompt at the input rate. Charging the whole prompt at the input rate is
// what this term exists to stop: on OpenAI's published table the cache-read rate
// is a tenth of the input rate, so the difference is the bulk of the bill for a
// cache-heavy workload.
func TestCalculateUSDTokensV1PricesTheCachedSpanAtTheCacheReadRate(t *testing.T) {
	price := validPriceVersion(t, "price_cached", 1, "2026-08-04T00:00:00Z")
	price.InputMicrosPerMillion = 5_000_000
	price.CachedInputMicrosPerMillion = 500_000
	price.OutputMicrosPerMillion = 0
	price.FixedRequestMicrosUSD = 0
	cost, err := CalculateUSDTokensV1(1_000_000, 800_000, 0, price)
	if err != nil {
		t.Fatal(err)
	}
	// 200k uncached at $5/M plus 800k cached at $0.50/M.
	if cost.InputCostMicrosUSD != 1_400_000 || cost.TotalCostMicrosUSD != 1_400_000 {
		t.Fatalf("cost=%#v", cost)
	}
	uncached, err := CalculateUSDTokensV1(1_000_000, 0, 0, price)
	if err != nil || uncached.TotalCostMicrosUSD != 5_000_000 {
		t.Fatalf("uncached cost=%#v err=%v", uncached, err)
	}
}

// The cache tiers partition the prompt; they are never added on top of it. A
// caller that reports more cached tokens than prompt tokens has mistranslated a
// provider's usage, and pricing it would undercharge the difference.
func TestCalculateUSDTokensV1RejectsCachedTokensExceedingTheInput(t *testing.T) {
	price := validPriceVersion(t, "price_cached_overrun", 1, "2026-08-04T00:00:00Z")
	price.CachedInputMicrosPerMillion = 40_000
	if _, err := CalculateUSDTokensV1(10, 11, 0, price); err == nil {
		t.Fatal("priced more cached tokens than the prompt contained")
	}
}

func TestParseUSDMicrosIsExactAndRejectsRounding(t *testing.T) {
	tests := map[string]int64{
		"0": 0, "0.40": 400_000, "1.000001": 1_000_001, "9223372036854.775807": math.MaxInt64,
	}
	for input, want := range tests {
		got, err := ParseUSDMicros(input)
		if err != nil || got != want {
			t.Fatalf("ParseUSDMicros(%q)=%d err=%v want=%d", input, got, err, want)
		}
	}
	for _, invalid := range []string{"", "-1", "+1", ".1", "1.", "1e3", "0.0000001", "9223372036854.775808"} {
		if _, err := ParseUSDMicros(invalid); err == nil {
			t.Fatalf("ParseUSDMicros(%q) unexpectedly succeeded", invalid)
		}
	}
}
