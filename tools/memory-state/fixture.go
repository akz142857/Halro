package main

import (
	"fmt"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

const fixtureVersion = "request-lifecycle-v1"

var fixtureStart = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

// visitFixture keeps only one request's five records alive. A full fixture slice
// would itself introduce the history-sized allocation this tool measures.
func visitFixture(requests int, priced bool, visit func(ledger.Record) error) error {
	for index := 0; index < requests; index++ {
		records, err := requestRecords(index, priced)
		if err != nil {
			return err
		}
		for _, record := range records {
			if err := visit(record); err != nil {
				return fmt.Errorf("sequence %d: %w", record.Sequence, err)
			}
		}
	}
	return nil
}

func requestRecords(index int, priced bool) ([5]ledger.Record, error) {
	var records [5]ledger.Record
	base := fixtureStart.Add(time.Duration(index) * 10 * time.Millisecond)
	common := ledger.Event{
		RequestID: fmt.Sprintf("req_%09d", index), AttemptID: fmt.Sprintf("att_%09d", index),
		ProjectID: "project_1", KeyID: "key_1", RouteID: "route_1", DeploymentID: "deployment_1",
		ProviderID: "provider_1", RequestedModel: "chat", ProviderModel: "synthetic-chat",
		PeriodID: base.Format("2006-01-02"), PeriodTimezone: "UTC", PeriodTimezoneVersion: 1,
		PeriodStartMicros: base.Truncate(24 * time.Hour).UnixMicro(),
		PeriodEndMicros:   base.Truncate(24 * time.Hour).Add(24 * time.Hour).UnixMicro(),
	}
	committed := int64(80)
	if priced {
		price := domain.DeploymentPriceVersion{
			ID: "price_1", DeploymentID: common.DeploymentID, Version: 1, Revision: 1,
			BillingMode: domain.BillingModeMetered, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1,
			InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
			EffectiveFrom: fixtureStart.Add(-time.Hour), CreatedAt: fixtureStart.Add(-time.Hour), CreatedBy: "fixture",
			Source: domain.PriceSource{Type: domain.PriceSourceManual, Assurance: domain.PriceAssuranceAsserted,
				ReceivedAt: fixtureStart.Add(-time.Hour), Reference: "synthetic fixture", AssertedWithoutArchive: true,
				ContentSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		}
		snapshot, err := domain.NewVersionedPriceSnapshot(price, base)
		if err != nil {
			return records, err
		}
		common.PriceSnapshot, common.LeaseMode = &snapshot, ledger.LeaseModeMetered
		common.PreparedInputTokens, common.PreparedOutputTokens = 7, 3
		common.TokenGuardPricingViewDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		committed = 13 // 7 input + 3 output at the frozen rates above.
	}
	for step := range records {
		event := common
		event.EventID = fmt.Sprintf("evt_%09d_%d", index, step)
		event.Kind = ledger.EventKind(step + 1)
		event.OccurredAt = base.Add(time.Duration(step) * time.Millisecond)
		switch event.Kind {
		case ledger.EventRequestAccepted, ledger.EventRequestFinalized:
			event.AttemptID, event.LeaseMode, event.PriceSnapshot = "", "", nil
		case ledger.EventReservationCreated:
			event.ReservationMicrosUSD = ledger.MicrosUSD(100)
		case ledger.EventAttemptSettled:
			event.CommittedMicrosUSD = ledger.MicrosUSD(committed)
			event.ProviderInputTokens, event.ProviderOutputTokens = 7, 3
			event.AttemptNumber, event.LatencyMillis = 1, 3
			event.TokenUsageSource = ledger.TokenUsageSourceProvider
		}
		if event.Kind == ledger.EventAttemptSettled || event.Kind == ledger.EventRequestFinalized {
			event.Outcome = "success"
		}
		if event.PriceSnapshot != nil {
			snapshot := event.PriceSnapshot.Clone()
			event.PriceSnapshot = &snapshot
		}
		sequence := uint64(index*5 + step + 1)
		records[step] = ledger.Record{Generation: 1, Sequence: sequence, Offset: int64(sequence * 256), Event: event}
	}
	return records, nil
}
