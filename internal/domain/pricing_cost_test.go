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
	cost, err := CalculateUSDTokensV1(1, 1, price)
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
	cost, err := CalculateUSDTokensV1(1_000_000, 0, price)
	if err != nil || cost.TotalCostMicrosUSD != math.MaxInt64 {
		t.Fatalf("wide intermediate cost=%#v err=%v", cost, err)
	}
	if _, err := CalculateUSDTokensV1(1_000_001, 0, price); err == nil {
		t.Fatal("expected final int64 overflow")
	}
}

func TestCalculateUSDTokensV1KeepsExplicitFreeKnown(t *testing.T) {
	price := validPriceVersion(t, "price_free_formula", 1, "2026-08-04T00:00:00Z")
	price.BillingMode = BillingModeFree
	price.InputMicrosPerMillion, price.OutputMicrosPerMillion = 0, 0
	cost, err := CalculateUSDTokensV1(math.MaxInt64, math.MaxInt64, price)
	if err != nil || cost != (PriceCostBreakdown{}) {
		t.Fatalf("free cost=%#v err=%v", cost, err)
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
