package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/domain"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func seedRuntimePricingPending(t *testing.T, s *boltstore.Store, dep string, n int) []domain.PricingAuditIntent {
	t.Helper()
	prices, err := s.ListDeploymentPriceVersions(context.Background(), dep)
	if err != nil || len(prices) == 0 {
		t.Fatal(prices, err)
	}
	base := time.Now().UTC().Truncate(time.Second)
	intents := make([]domain.PricingAuditIntent, 0, n)
	for i := 0; i < n; i++ {
		p := prices[0]
		p.ID = fmt.Sprintf("price_pending_%05d", i)
		p.Version, p.Revision = 0, 0
		p.CreatedAt, p.Source.ReceivedAt = base, base
		p.EffectiveFrom = base.Add(time.Duration(i+1) * time.Hour)
		intent := domain.PricingAuditIntent{EventID: fmt.Sprintf("event_pending_%05d", i), OccurredAt: base.Add(-time.Duration(i+1) * time.Second), ActorID: "system", Action: "deployment_price.create", TargetType: "deployment_price_version", TargetID: p.ID, RequestSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
		intent.RecordSource(p.Source)
		_, saved, _, err := s.CreateDeploymentPriceVersionIdempotent(context.Background(), p, intent, fmt.Sprintf("sha256:%064x", i+1))
		if err != nil {
			t.Fatal(err)
		}
		intents = append(intents, saved)
	}
	return intents
}

func TestPricingPendingRuntimeStartupKeepsEventTimeAndReopenDoesNotDuplicate(t *testing.T) {
	r, boot, _, _ := openPricingRuntimeForTest(t, "pending-runtime")
	intents := seedRuntimePricingPending(t, r.store, boot.DeploymentID, 3)
	// Crash boundary: Audit is durable but its Metadata delivered marker is absent.
	intent := intents[2]
	if _, err := r.audit.Append(context.Background(), audit.Event{
		EventID: intent.EventID, OccurredAt: intent.OccurredAt, ActorType: "admin_user", ActorID: intent.ActorID,
		Action: intent.Action, TargetType: intent.TargetType, TargetID: intent.TargetID, Outcome: "success", CorrelationID: intent.CorrelationID,
		Metadata: map[string]any{"deployment_id": intent.DeploymentID, "price_version": intent.PriceVersion, "effective_from": intent.EffectiveFrom,
			"request_sha256": intent.RequestSHA256, "source_type": intent.SourceType, "source_assurance": intent.SourceAssurance, "source_content_sha256": intent.SourceContentSHA256,
			"source_reference": intent.SourceReference, "source_without_archive": intent.SourceWithoutArchive, "change_summary": intent.ChangeSummary},
	}); err != nil {
		t.Fatal(err)
	}
	intents = []domain.PricingAuditIntent{intents[2], intents[0], intents[1]}
	cfg := r.config
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 2; round++ {
		next, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		count, err := next.store.PendingPricingAuditIntentCount(context.Background())
		if err != nil || count != 0 {
			t.Fatal(count, err)
		}
		seen := 0
		_, err = next.audit.Replay(func(record audit.Record) error {
			if !strings.HasPrefix(record.Event.EventID, "event_pending_") {
				return nil
			}
			if seen >= len(intents) || record.Event.EventID != intents[seen].EventID || !record.Event.OccurredAt.Equal(intents[seen].OccurredAt) {
				t.Fatal("receipt changed identity/time or duplicated", record, seen)
			}
			seen++
			return nil
		})
		if err != nil || seen != 3 {
			t.Fatal(seen, err)
		}
		if err := next.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPricingPendingMetricsReadCountAndOmitUnavailableStore(t *testing.T) {
	r, boot, _, _ := openPricingRuntimeForTest(t, "pending-metrics")
	intents := seedRuntimePricingPending(t, r.store, boot.DeploymentID, 3)
	for _, want := range []string{"3", "2"} {
		body := renderMetricsForTest(t, r)
		if !strings.Contains(body, "halro_pricing_recovery_pending_intents "+want+"\n") {
			t.Fatal(body)
		}
		if want == "3" {
			if err := r.store.MarkPricingAuditIntentDelivered(context.Background(), intents[0].EventID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := r.store.Close(); err != nil {
		t.Fatal(err)
	}
	if body := renderMetricsForTest(t, r); strings.Contains(body, "halro_pricing_recovery_pending_intents") {
		t.Fatal("unavailable count rendered as zero", body)
	}
}
