package domain

import (
	"errors"
	"strings"
	"time"
)

type PricePinState string

const (
	PricePinPrepared  PricePinState = "prepared"
	PricePinCommitted PricePinState = "committed"
)

type DeploymentPricingHighWater struct {
	DeploymentID                 string    `json:"deployment_id"`
	LatestObservedPriceVersionID string    `json:"latest_observed_price_version_id"`
	LatestSelectedAt             time.Time `json:"latest_selected_at"`
	LatestObservedEffectiveFrom  time.Time `json:"latest_observed_effective_from"`
	Quarantined                  bool      `json:"quarantined"`
	QuarantineReason             string    `json:"quarantine_reason,omitempty"`
	Revision                     uint64    `json:"revision"`
}

func (h DeploymentPricingHighWater) Validate() error {
	if strings.TrimSpace(h.DeploymentID) == "" || strings.TrimSpace(h.LatestObservedPriceVersionID) == "" || h.LatestSelectedAt.IsZero() || h.LatestSelectedAt.Location() != time.UTC ||
		h.LatestObservedEffectiveFrom.IsZero() || h.LatestObservedEffectiveFrom.Location() != time.UTC || h.Revision == 0 {
		return errors.New("deployment pricing high-water is invalid")
	}
	if h.LatestObservedEffectiveFrom.After(h.LatestSelectedAt) {
		return errors.New("pricing high-water effective time is after selection time")
	}
	if h.Quarantined != (strings.TrimSpace(h.QuarantineReason) != "") {
		return errors.New("pricing quarantine reason and state must be set together")
	}
	return nil
}

type PricePinIntent struct {
	AttemptID         string        `json:"attempt_id"`
	DeploymentID      string        `json:"deployment_id"`
	PriceVersionID    string        `json:"price_version_id"`
	PriceVersion      uint64        `json:"price_version"`
	SnapshotSHA256    string        `json:"snapshot_sha256"`
	PricingSelectedAt time.Time     `json:"pricing_selected_at"`
	MetadataRevision  uint64        `json:"metadata_revision"`
	State             PricePinState `json:"state"`
	LedgerSequence    uint64        `json:"ledger_sequence,omitempty"`
	CreatedAt         time.Time     `json:"created_at"`
	CommittedAt       *time.Time    `json:"committed_at,omitempty"`
}

func (p PricePinIntent) Validate() error {
	if strings.TrimSpace(p.AttemptID) == "" || strings.TrimSpace(p.DeploymentID) == "" || strings.TrimSpace(p.PriceVersionID) == "" ||
		p.PriceVersion == 0 || !validSHA256Label(p.SnapshotSHA256) || p.PricingSelectedAt.IsZero() || p.PricingSelectedAt.Location() != time.UTC ||
		p.MetadataRevision == 0 || p.CreatedAt.IsZero() || p.CreatedAt.Location() != time.UTC {
		return errors.New("price pin intent is invalid")
	}
	switch p.State {
	case PricePinPrepared:
		if p.LedgerSequence != 0 || p.CommittedAt != nil {
			return errors.New("prepared price pin cannot contain commit metadata")
		}
	case PricePinCommitted:
		if p.LedgerSequence == 0 || p.CommittedAt == nil || p.CommittedAt.IsZero() || p.CommittedAt.Location() != time.UTC {
			return errors.New("committed price pin requires UTC commit metadata")
		}
	default:
		return errors.New("price pin state is invalid")
	}
	return nil
}
