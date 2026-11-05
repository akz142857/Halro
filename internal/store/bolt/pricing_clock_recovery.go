package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

func clockPricingReviewTx(tx *Tx, deploymentID string, now time.Time) (domain.PricingClockReview, error) {
	var review domain.PricingClockReview
	if tx.Bucket(bucketDeployments).Get([]byte(deploymentID)) == nil {
		return review, ErrNotFound
	}
	deleted, err := pricingDeploymentDeletedTx(tx, deploymentID)
	if err != nil {
		return review, err
	}
	if deleted {
		return review, ErrNotFound
	}
	raw := tx.Bucket(bucketDeploymentPricingHighWater).Get([]byte(deploymentID))
	if raw == nil {
		return review, ErrNotFound
	}
	var high domain.DeploymentPricingHighWater
	if err := json.Unmarshal(raw, &high); err != nil {
		return review, err
	}
	if err := high.Validate(); err != nil {
		return review, err
	}
	if !high.Quarantined || !domain.IsClockPricingQuarantine(high.QuarantineReason) {
		return review, ErrRevisionConflict
	}
	timeline := tx.Bucket(bucketDeploymentPriceTimeline).Bucket([]byte(deploymentID))
	if timeline == nil {
		return review, domain.ErrPriceUnavailable
	}
	var prices []domain.DeploymentPriceVersion
	if err := timeline.ForEach(func(_, id []byte) error {
		var price domain.DeploymentPriceVersion
		raw := tx.Bucket(bucketDeploymentPriceVersions).Get(id)
		if raw == nil {
			return errors.New("price timeline references missing price")
		}
		if err := json.Unmarshal(raw, &price); err != nil {
			return err
		}
		prices = append(prices, price)
		return nil
	}); err != nil {
		return review, err
	}
	if err := domain.AuditPriceTimeline(prices, deploymentID); err != nil {
		return review, err
	}
	price, err := domain.SelectAuditedPriceVersion(prices, now)
	if err != nil {
		return review, err
	}
	return domain.PricingClockReview{DeploymentID: deploymentID, PreviousSelectedAt: high.LatestSelectedAt, Price: price,
		Confirmation: domain.PricingClockConfirmation{Revision: high.Revision, Reason: high.QuarantineReason, PriceID: price.ID, ReviewedAt: now}}, nil
}

func (s *Store) ReviewClockPricing(ctx context.Context, deploymentID string, now time.Time) (domain.PricingClockReview, error) {
	var review domain.PricingClockReview
	if err := ctx.Err(); err != nil {
		return review, err
	}
	if deploymentID == "" || now.IsZero() || now.Location() != time.UTC {
		return review, errors.New("deployment and UTC review time required")
	}
	err := s.view(func(tx *Tx) error {
		var err error
		review, err = clockPricingReviewTx(tx, deploymentID, now)
		return err
	})
	return review, err
}

// RecoverClockPricingWithAuditIntent owns the exclusive pricing gate, unlike
// the older restore operation. No pin may straddle the durable reset and the
// in-memory anchor reset. Existing pins, leases and historical prices stay intact.
func (s *Store) RecoverClockPricingWithAuditIntent(ctx context.Context, deploymentID string, expected domain.PricingClockConfirmation, now time.Time, intent domain.PricingAuditIntent) (domain.PricingAuditIntent, error) {
	if err := ctx.Err(); err != nil {
		return intent, err
	}
	if err := intent.Validate(); err != nil {
		return intent, err
	}
	if intent.Action != "deployment_price.clock_confirm" || intent.TargetType != "deployment_pricing" || intent.TargetID != deploymentID || intent.DeploymentID != deploymentID {
		return intent, errors.New("audit intent does not match clock recovery")
	}
	if now.IsZero() || now.Location() != time.UTC || expected.ReviewedAt.IsZero() || expected.ReviewedAt.Location() != time.UTC || now.Before(expected.ReviewedAt) || now.Sub(expected.ReviewedAt) > 5*time.Minute {
		return intent, ErrRevisionConflict
	}
	unlock := s.LockDeploymentPricingExclusive(deploymentID)
	defer unlock()
	err := s.update(func(tx *Tx) error {
		review, err := clockPricingReviewTx(tx, deploymentID, now)
		if err != nil {
			return err
		}
		if expected.Revision != review.Confirmation.Revision || expected.Reason != review.Confirmation.Reason || expected.PriceID != review.Price.ID {
			return ErrRevisionConflict
		}
		high := domain.DeploymentPricingHighWater{DeploymentID: deploymentID, LatestObservedPriceVersionID: review.Price.ID, LatestObservedEffectiveFrom: review.Price.EffectiveFrom, LatestSelectedAt: now, Revision: expected.Revision + 1}
		if err := high.Validate(); err != nil {
			return err
		}
		var previous domain.DeploymentPricingHighWater
		if err := json.Unmarshal(tx.Bucket(bucketDeploymentPricingHighWater).Get([]byte(deploymentID)), &previous); err != nil {
			return err
		}
		summary, err := json.Marshal(map[string]any{"before": previous, "after": high})
		if err != nil {
			return err
		}
		intent.ChangeSummary = string(summary)
		intent.PriceVersion = review.Price.Version
		effective := review.Price.EffectiveFrom
		intent.EffectiveFrom = &effective
		if err := intent.Validate(); err != nil {
			return err
		}
		encoded, err := json.Marshal(high)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketDeploymentPricingHighWater).Put([]byte(deploymentID), encoded); err != nil {
			return err
		}
		return putPricingAuditIntentTx(tx, intent)
	})
	if err == nil {
		state := s.deploymentPricingState(deploymentID)
		state.clockMu.Lock()
		state.clock = pricingClockObservation{}
		state.hasClock = false
		state.clockMu.Unlock()
	}
	return intent, err
}
