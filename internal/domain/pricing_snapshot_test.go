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
