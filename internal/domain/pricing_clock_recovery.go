package domain

import "time"

// PricingClockConfirmation binds the operator's review to one quarantine and
// one immutable price. A new price or quarantine requires a new review.
type PricingClockConfirmation struct {
	Revision   uint64    `json:"revision"`
	Reason     string    `json:"reason"`
	PriceID    string    `json:"price_id"`
	ReviewedAt time.Time `json:"reviewed_at"`
}

type PricingClockReview struct {
	DeploymentID       string                   `json:"deployment_id"`
	Confirmation       PricingClockConfirmation `json:"confirmation"`
	PreviousSelectedAt time.Time                `json:"previous_selected_at"`
	Price              DeploymentPriceVersion   `json:"price"`
}

func IsClockPricingQuarantine(reason string) bool {
	return reason == "wall_clock_forward_jump" || reason == "wall_clock_rollback"
}
