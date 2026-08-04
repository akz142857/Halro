package domain

import (
	"testing"
	"time"
)

func TestPriceSnapshotDistinguishesFreeFromUnknownAndIsSelfContained(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	free := validPriceVersion(t, "price_free", 1, now.Add(-time.Hour).Format(time.RFC3339))
	free.DeploymentID = "dep"
	free.BillingMode = BillingModeFree
	free.InputMicrosPerMillion, free.OutputMicrosPerMillion, free.FixedRequestMicrosUSD = 0, 0, 0
	snapshot, err := NewVersionedPriceSnapshot(free, now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.FixedRequestMicrosUSD == nil || *snapshot.FixedRequestMicrosUSD != 0 || snapshot.CostValueStatus != CostValueKnown {
		t.Fatalf("free snapshot=%#v", snapshot)
	}
	cost, err := snapshot.Calculate(100, 200)
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
