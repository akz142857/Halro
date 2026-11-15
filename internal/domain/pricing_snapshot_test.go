package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPriceSnapshotDistinguishesFreeFromUnknownAndIsSelfContained(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	free := validPriceVersion(t, "price_free", 1, now.Add(-time.Hour).Format(time.RFC3339))
	free.DeploymentID = "dep"
	free.BillingMode = BillingModeFree
	free.InputMicrosPerMillion, free.CachedInputMicrosPerMillion, free.OutputMicrosPerMillion, free.FixedRequestMicrosUSD = 0, 0, 0, 0
	snapshot, err := NewVersionedPriceSnapshot(free, now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.FixedRequestMicrosUSD == nil || *snapshot.FixedRequestMicrosUSD != 0 || snapshot.CostValueStatus != CostValueKnown {
		t.Fatalf("free snapshot=%#v", snapshot)
	}
	cost, err := snapshot.Calculate(BilledTokens{Input: 100, CachedInput: 10, Output: 200})
	if err != nil || cost.TotalCostMicrosUSD != 0 {
		t.Fatalf("free cost=%#v err=%v", cost, err)
	}
	unknown := NewUnknownPriceSnapshot(now)
	if err := unknown.Validate(); err != nil || unknown.FixedRequestMicrosUSD != nil || unknown.CostValueStatus != CostValueUnknown {
		t.Fatalf("unknown snapshot=%#v err=%v", unknown, err)
	}
}

func TestPriceSnapshotRejectsSelectionBeforeEffectiveTime(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	price := validPriceVersion(t, "price_future", 1, now.Add(time.Hour).Format(time.RFC3339))
	price.DeploymentID = "dep"
	if _, err := NewVersionedPriceSnapshot(price, now); err == nil {
		t.Fatal("accepted a price snapshot before effective_from")
	}
}

func TestPriceSnapshotPreservesManualSourceWithoutArchive(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	price := validPriceVersion(t, "price_manual", 2, now.Add(-time.Hour).Format(time.RFC3339))
	price.DeploymentID = "dep"
	price.Source.Type = PriceSourceManual
	price.Source.URI = ""
	price.Source.RetrievedAt = nil
	price.Source.ContentSHA256 = ""
	price.Source.Reference = "internal_cost"
	price.Source.AssertedWithoutArchive = true
	snapshot, err := NewVersionedPriceSnapshot(price, now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SourceReference != "internal_cost" || !snapshot.SourceWithoutArchive || snapshot.SourceContentSHA256 != "" {
		t.Fatalf("manual source evidence was not preserved: %#v", snapshot)
	}
	withoutAssertion := snapshot
	withoutAssertion.SourceWithoutArchive = false
	if err := withoutAssertion.Validate(); err == nil {
		t.Fatal("manual snapshot without a digest or explicit no-archive assertion was accepted")
	}
	whitespaceReference := snapshot
	whitespaceReference.SourceReference = "   "
	if err := whitespaceReference.Validate(); err == nil {
		t.Fatal("manual snapshot with a whitespace-only source reference was accepted")
	}
}

// A snapshot decoded from a record written before the cache-read term existed
// has no rate for the cached span. Reading the missing term as zero would price
// every cached token free, so the snapshot is refused instead — the same
// fail-closed answer the other billing terms already give.
func TestPriceSnapshotRefusesAMissingCacheReadRate(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	price := validPriceVersion(t, "price_cached_missing", 1, now.Add(-time.Hour).Format(time.RFC3339))
	price.DeploymentID = "dep"
	price.CachedInputMicrosPerMillion = 40_000
	snapshot, err := NewVersionedPriceSnapshot(price, now)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "cached_input_micros_per_million")
	legacy, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var decoded PriceSnapshot
	if err := json.Unmarshal(legacy, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(); err == nil {
		t.Fatal("a snapshot without a cache-read rate validated")
	}
	if _, err := decoded.Calculate(BilledTokens{Input: 1_000, CachedInput: 500, Output: 0}); err == nil {
		t.Fatal("priced an attempt against a snapshot with no cache-read rate")
	}
}

func TestLegacyPriceSnapshotAcceptsAbsentWriteRates(t *testing.T) {
	price := validPriceVersion(t, "price_legacy_write", 1, "2026-08-04T00:00:00Z")
	snapshot, err := NewVersionedPriceSnapshot(price, priceTestInstant)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.CacheWriteMicrosPerMillion, snapshot.CacheWriteLongMicrosPerMillion = nil, nil
	wire, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var legacy PriceSnapshot
	if err := json.Unmarshal(wire, &legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Calculate(BilledTokens{Input: 100}); err != nil {
		t.Fatalf("old snapshot: %v", err)
	}
	if legacy.CacheWriteMicrosPerMillion != nil || legacy.Tier().CacheWriteLongMicrosPerMillion != 0 {
		t.Fatal("absent legacy rates did not decode to zero")
	}
	legacy.CacheWriteLongMicrosPerMillion = legacy.InputMicrosPerMillion
	if err := legacy.Validate(); err == nil {
		t.Fatal("snapshot accepted an incomplete pair of write rates")
	}
}

func TestLegacySnapshotKeepsThePinnedBillingRule(t *testing.T) {
	price := validPriceVersion(t, "price_legacy_cost", 1, "2026-08-04T00:00:00Z")
	price.InputMicrosPerMillion = 400_000
	price.CachedInputMicrosPerMillion = 2_000_000
	price.OutputMicrosPerMillion = 0
	legacy, err := NewVersionedPriceSnapshot(price, priceTestInstant)
	if err != nil {
		t.Fatal(err)
	}
	legacy.CacheWriteMicrosPerMillion, legacy.CacheWriteLongMicrosPerMillion = nil, nil
	cost, err := legacy.Calculate(BilledTokens{Input: 4, CachedInput: 1, CacheWrite: 2, CacheWriteLong: 1})
	if err != nil || cost.InputCostMicrosUSD != 4 {
		t.Fatalf("legacy cost=%+v err=%v", cost, err)
	}
	recovered := legacy.ConservativeTokens(4, 0)
	bound, err := legacy.ReservationCost(4, 0)
	if err != nil || recovered.CachedInput != 0 || bound != 2+price.FixedRequestMicrosUSD {
		t.Fatalf("legacy estimated split=%+v cost=%d err=%v", recovered, bound, err)
	}
}
