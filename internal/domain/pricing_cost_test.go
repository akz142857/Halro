package domain

import (
	"math"
	"testing"
	"time"
)

// priceTestInstant is a fixed instant for prices that bill one rate around the
// clock: they resolve to the same tier whatever it is, and pinning it keeps
// these cases from depending on when the suite runs.
var priceTestInstant = time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)

func TestCalculateUSDTokensV1RoundsComponentsIndependently(t *testing.T) {
	price := validPriceVersion(t, "price_formula", 1, "2026-08-04T00:00:00Z")
	price.InputMicrosPerMillion = 400_000
	price.OutputMicrosPerMillion = 1_600_000
	price.FixedRequestMicrosUSD = 3
	cost, err := CalculateUSDTokensV1(BilledTokens{Input: 1, CachedInput: 0, Output: 1}, price, priceTestInstant)
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
	cost, err := CalculateUSDTokensV1(BilledTokens{Input: 1_000_000, CachedInput: 0, Output: 0}, price, priceTestInstant)
	if err != nil || cost.TotalCostMicrosUSD != math.MaxInt64 {
		t.Fatalf("wide intermediate cost=%#v err=%v", cost, err)
	}
	if _, err := CalculateUSDTokensV1(BilledTokens{Input: 1_000_001, CachedInput: 0, Output: 0}, price, priceTestInstant); err == nil {
		t.Fatal("expected final int64 overflow")
	}
}

func TestCalculateUSDTokensV1KeepsExplicitFreeKnown(t *testing.T) {
	price := validPriceVersion(t, "price_free_formula", 1, "2026-08-04T00:00:00Z")
	price.BillingMode = BillingModeFree
	price.InputMicrosPerMillion, price.CachedInputMicrosPerMillion, price.OutputMicrosPerMillion = 0, 0, 0
	cost, err := CalculateUSDTokensV1(BilledTokens{Input: math.MaxInt64, CachedInput: math.MaxInt64, Output: math.MaxInt64}, price, priceTestInstant)
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
	cost, err := CalculateUSDTokensV1(BilledTokens{Input: 1_000_000, CachedInput: 800_000, Output: 0}, price, priceTestInstant)
	if err != nil {
		t.Fatal(err)
	}
	// 200k uncached at $5/M plus 800k cached at $0.50/M.
	if cost.InputCostMicrosUSD != 1_400_000 || cost.TotalCostMicrosUSD != 1_400_000 {
		t.Fatalf("cost=%#v", cost)
	}
	uncached, err := CalculateUSDTokensV1(BilledTokens{Input: 1_000_000, CachedInput: 0, Output: 0}, price, priceTestInstant)
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
	if _, err := CalculateUSDTokensV1(BilledTokens{Input: 10, CachedInput: 11, Output: 0}, price, priceTestInstant); err == nil {
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

func TestCacheWritesPriceBothLifetimesAndRoundEachTier(t *testing.T) {
	price := validPriceVersion(t, "price_cache_write", 1, "2026-08-04T00:00:00Z")
	price.InputMicrosPerMillion = 400_000
	price.CachedInputMicrosPerMillion = 100_000
	price.CacheWriteMicrosPerMillion = 1_250_000
	price.CacheWriteLongMicrosPerMillion = 2_100_000
	price.OutputMicrosPerMillion = 1_600_000
	price.FixedRequestMicrosUSD = 3
	snapshot, err := NewVersionedPriceSnapshot(price, priceTestInstant)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		tokens    BilledTokens
		inputCost int64
	}{
		{"mixed", BilledTokens{Input: 8, CachedInput: 3, CacheWrite: 4, CacheWriteLong: 3, Output: 1}, 11},
		{"writes all long", BilledTokens{Input: 8, CachedInput: 3, CacheWrite: 4, CacheWriteLong: 4, Output: 1}, 11},
		{"no writes", BilledTokens{Input: 8, CachedInput: 3, Output: 1}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cost, err := CalculateUSDTokensV1(tc.tokens, price, priceTestInstant)
			frozen, frozenErr := snapshot.Calculate(tc.tokens)
			if err != nil || frozenErr != nil || cost != frozen || cost.InputCostMicrosUSD != tc.inputCost || cost.TotalCostMicrosUSD != tc.inputCost+5 {
				t.Fatalf("cost=%+v snapshot=%+v err=%v snapshotErr=%v", cost, frozen, err, frozenErr)
			}
		})
	}
	if _, err := snapshot.Calculate(BilledTokens{Input: 8, CacheWrite: 3, CacheWriteLong: 4}); err == nil {
		t.Fatal("long writes exceeding total were priced")
	}
	free := price
	free.BillingMode = BillingModeFree
	free.InputMicrosPerMillion, free.CachedInputMicrosPerMillion = 0, 0
	free.OutputMicrosPerMillion, free.FixedRequestMicrosUSD = 0, 0
	if err := free.Validate(); err == nil {
		t.Fatal("free price accepted nonzero write rates")
	}
}

func TestReservationCostBoundsIndependentRoundingForEverySplit(t *testing.T) {
	for _, tier := range []PriceTier{
		{InputMicrosPerMillion: 4, CachedInputMicrosPerMillion: 3, CacheWriteMicrosPerMillion: 2, CacheWriteLongMicrosPerMillion: 5},
		{InputMicrosPerMillion: 1_000_000, CachedInputMicrosPerMillion: 1_400_000, CacheWriteMicrosPerMillion: 1_250_000, CacheWriteLongMicrosPerMillion: 1_500_000},
		{InputMicrosPerMillion: 4, CachedInputMicrosPerMillion: 5_100_000, CacheWriteMicrosPerMillion: 0, CacheWriteLongMicrosPerMillion: 3_000_000},
	} {
		for input := int64(0); input <= 10; input++ {
			reserved, err := tier.ReservationCost(input, 0)
			if err != nil {
				t.Fatal(err)
			}
			for cached := int64(0); cached <= input; cached++ {
				for write := int64(0); write <= input-cached; write++ {
					for long := int64(0); long <= write; long++ {
						split := BilledTokens{Input: input, CachedInput: cached, CacheWrite: write, CacheWriteLong: long}
						cost, err := tier.Calculate(split)
						if err != nil || cost.TotalCostMicrosUSD > reserved {
							t.Fatalf("tier=%+v split=%+v cost=%+v reserve=%d err=%v", tier, split, cost, reserved, err)
						}
					}
				}
			}
		}
	}
}

func TestBackfilledCacheWritesKeepTheOldRoundedCharge(t *testing.T) {
	tier := PriceTier{
		InputMicrosPerMillion: 400_000, CachedInputMicrosPerMillion: 40_000,
		CacheWriteMicrosPerMillion: 400_000, CacheWriteLongMicrosPerMillion: 400_000,
		OutputMicrosPerMillion: 1_600_000, FixedRequestMicrosUSD: 2,
	}
	ordinary := BilledTokens{Input: 4, CachedInput: 1, Output: 1}
	withWrites := BilledTokens{Input: 4, CachedInput: 1, CacheWrite: 2, CacheWriteLong: 1, Output: 1}
	want, err := tier.Calculate(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tier.Calculate(withWrites)
	if err != nil || got != want || got.InputCostMicrosUSD != 3 {
		t.Fatalf("ordinary=%+v mixed=%+v err=%v", want, got, err)
	}
}

func TestPriceTierRejectsNegativeRatesBeforePricing(t *testing.T) {
	for _, tier := range []PriceTier{
		{InputMicrosPerMillion: -1, OutputMicrosPerMillion: 1_000_000},
		{CacheWriteMicrosPerMillion: -1}, {CacheWriteLongMicrosPerMillion: -1},
		{FixedRequestMicrosUSD: -1},
	} {
		if _, err := tier.ReservationCost(2, 10); err == nil {
			t.Fatalf("accepted negative rate in %+v", tier)
		}
	}
}
