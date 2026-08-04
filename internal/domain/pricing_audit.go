package domain

import (
	"errors"
	"strings"
	"time"
)

type PricingAuditIntent struct {
	EventID              string               `json:"event_id"`
	OccurredAt           time.Time            `json:"occurred_at"`
	ActorID              string               `json:"actor_id"`
	Action               string               `json:"action"`
	TargetType           string               `json:"target_type"`
	TargetID             string               `json:"target_id"`
	CorrelationID        string               `json:"correlation_id,omitempty"`
	RequestSHA256        string               `json:"request_sha256"`
	Delivered            bool                 `json:"delivered"`
	DeploymentID         string               `json:"deployment_id,omitempty"`
	PriceVersion         uint64               `json:"price_version,omitempty"`
	EffectiveFrom        *time.Time           `json:"effective_from,omitempty"`
	SourceType           PriceSourceType      `json:"source_type,omitempty"`
	SourceAssurance      PriceSourceAssurance `json:"source_assurance,omitempty"`
	SourceContentSHA256  string               `json:"source_content_sha256,omitempty"`
	SourceReference      string               `json:"source_reference,omitempty"`
	SourceWithoutArchive bool                 `json:"source_without_archive,omitempty"`
	ChangeSummary        string               `json:"change_summary,omitempty"`
}

func (i PricingAuditIntent) Validate() error {
	if strings.TrimSpace(i.EventID) == "" || i.OccurredAt.IsZero() || !isUTC(i.OccurredAt) ||
		strings.TrimSpace(i.ActorID) == "" || strings.TrimSpace(i.TargetID) == "" ||
		!validSHA256Label(i.RequestSHA256) {
		return errors.New("invalid pricing audit intent")
	}
	if i.TargetType != "deployment_price_version" && i.TargetType != "deployment_price_proposal" && i.TargetType != "deployment_pricing" {
		return errors.New("pricing audit intent target type is invalid")
	}
	switch i.Action {
	case "deployment_price.create", "deployment_price.cancel", "deployment_price.restore_confirm",
		"deployment_price.proposal_create", "deployment_price.proposal_adopt", "deployment_price.proposal_reject", "deployment_price.migrate":
	default:
		return errors.New("pricing audit intent action is invalid")
	}
	if len(i.CorrelationID) > 256 {
		return errors.New("pricing audit intent correlation id exceeds maximum size")
	}
	if len(i.ChangeSummary) > 2048 || len(i.SourceReference) > 256 ||
		(i.SourceContentSHA256 != "" && !validSHA256Label(i.SourceContentSHA256)) {
		return errors.New("pricing audit evidence is invalid")
	}
	return nil
}
