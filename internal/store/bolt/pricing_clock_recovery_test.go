package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

func clockTestIntent(event string) domain.PricingAuditIntent {
	return domain.PricingAuditIntent{EventID: event, OccurredAt: time.Now().UTC(), ActorID: "operator", Action: "deployment_price.clock_confirm", TargetType: "deployment_pricing", TargetID: "dep_clock", DeploymentID: "dep_clock", RequestSHA256: "sha256:" + strings.Repeat("a", 64)}
}

func TestClockPricingRecovery(t *testing.T) {
	for _, reason := range []string{"wall_clock_forward_jump", "wall_clock_rollback"} {
		t.Run(reason, func(t *testing.T) {
			s, err := openForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			seedPricingDeployment(t, s, "dep_clock", 0, 0, 0)
			price, err := s.CreateDeploymentPriceVersion(ctx, newStoredPrice("price_clock", "dep_clock", now.Add(-time.Hour)))
			if err != nil {
				t.Fatal(err)
			}
			initial := now
			if reason == "wall_clock_rollback" {
				initial = now.Add(2 * time.Minute)
			}
			if _, _, _, err := s.PrepareDeploymentPricePin(ctx, "dep_clock", "att_original", initial, time.Now(), time.Second, 30*time.Second); err != nil {
				t.Fatal(err)
			}
			jumped := now.Add(2 * time.Minute)
			if reason == "wall_clock_rollback" {
				jumped = now
			}
			if _, _, _, err := s.PrepareDeploymentPricePin(ctx, "dep_clock", "att_jump", jumped, time.Now(), time.Second, 30*time.Second); !errors.Is(err, ErrPricingQuarantined) {
				t.Fatal(err)
			}
			review, err := s.ReviewClockPricing(ctx, "dep_clock", now)
			if err != nil {
				t.Fatal(err)
			}
			if review.Confirmation.Reason != reason || review.Price.ID != price.ID {
				t.Fatalf("review=%+v", review)
			}
			var oldPin []byte
			s.view(func(tx *Tx) error {
				oldPin = append([]byte(nil), tx.Bucket(bucketDeploymentPricePins).Get([]byte("att_original"))...)
				return nil
			})
			for _, change := range []func(*domain.PricingClockConfirmation){
				func(v *domain.PricingClockConfirmation) { v.Revision++ }, func(v *domain.PricingClockConfirmation) { v.PriceID = "stale" },
				func(v *domain.PricingClockConfirmation) { v.Reason = "restored_scheduled_price_requires_confirmation" },
				func(v *domain.PricingClockConfirmation) { v.ReviewedAt = now.Add(-6 * time.Minute) }, func(v *domain.PricingClockConfirmation) { v.ReviewedAt = now.Add(time.Minute) },
			} {
				expected := review.Confirmation
				change(&expected)
				if _, err := s.RecoverClockPricingWithAuditIntent(ctx, "dep_clock", expected, now, clockTestIntent("aud_bad")); !errors.Is(err, ErrRevisionConflict) {
					t.Fatalf("stale review accepted: %v", err)
				}
			}
			// Recovery waits for the in-flight lease gate rather than racing its pins.
			release := s.LockDeploymentPricingShared("dep_clock")
			done := make(chan error, 1)
			go func() {
				_, err := s.RecoverClockPricingWithAuditIntent(ctx, "dep_clock", review.Confirmation, now, clockTestIntent("aud_clock"))
				done <- err
			}()
			select {
			case err := <-done:
				t.Fatalf("recovery crossed an active pricing gate: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			release()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if err := s.PricingReadiness(ctx); err != nil {
				t.Fatal(err)
			}
			if _, has := s.deploymentPricingState("dep_clock").clockAnchor(); has {
				t.Fatal("stale clock anchor retained")
			}
			if err := s.view(func(tx *Tx) error {
				var high domain.DeploymentPricingHighWater
				if err := json.Unmarshal(tx.Bucket(bucketDeploymentPricingHighWater).Get([]byte("dep_clock")), &high); err != nil {
					return err
				}
				if !high.LatestSelectedAt.Equal(now) || high.LatestObservedPriceVersionID != price.ID || high.Revision != review.Confirmation.Revision+1 {
					t.Fatalf("high=%+v", high)
				}
				if string(tx.Bucket(bucketDeploymentPricePins).Get([]byte("att_original"))) != string(oldPin) {
					t.Fatal("historical pin modified")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			intents, err := s.ListPendingPricingAuditIntents(ctx)
			if err != nil || len(intents) != 1 || intents[0].ChangeSummary == "" {
				t.Fatalf("audit=%+v err=%v", intents, err)
			}
			if _, _, _, err := s.PrepareDeploymentPricePin(ctx, "dep_clock", "att_after", now.Add(time.Millisecond), time.Now(), time.Second, 30*time.Second); err != nil {
				t.Fatalf("recovered pin: %v", err)
			}
			if _, _, _, err := s.PrepareDeploymentPricePin(ctx, "dep_clock", "att_new_jump", now.Add(3*time.Minute), time.Now(), time.Second, 30*time.Second); !errors.Is(err, ErrPricingQuarantined) {
				t.Fatalf("protection disabled after recovery: %v", err)
			}
		})
	}
}

func TestClockPricingRecoveryRejectsChangedEffectivePrice(t *testing.T) {
	s, err := openForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	seedPricingDeployment(t, s, "dep_clock", 0, 0, 0)
	for _, p := range []domain.DeploymentPriceVersion{newStoredPrice("price_old", "dep_clock", now.Add(-time.Hour)), newStoredPrice("price_scheduled", "dep_clock", now.Add(time.Minute))} {
		if _, err := s.CreateDeploymentPriceVersion(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	s.PrepareDeploymentPricePin(ctx, "dep_clock", "att_before", now, time.Now(), time.Second, 30*time.Second)
	s.PrepareDeploymentPricePin(ctx, "dep_clock", "att_jump", now.Add(2*time.Minute), time.Now(), time.Second, 30*time.Second)
	review, err := s.ReviewClockPricing(ctx, "dep_clock", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverClockPricingWithAuditIntent(ctx, "dep_clock", review.Confirmation, now.Add(2*time.Minute), clockTestIntent("aud_stale")); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("scheduled boundary accepted: %v", err)
	}
	if quarantined, _, _ := s.DeploymentPricingQuarantine(ctx, "dep_clock"); !quarantined {
		t.Fatal("quarantine lost on conflict")
	}
	intents, _ := s.ListPendingPricingAuditIntents(ctx)
	if len(intents) != 0 {
		t.Fatal("audit written for refused recovery")
	}
}
