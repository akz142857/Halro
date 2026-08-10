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

// RecordSource copies onto the intent the evidence about where a price's
// numbers came from. It is the single place the audit trail learns this, so no
// caller has to remember which of the five fields belong together.
//
// The evidence is read off the price source itself and never inferred from the
// shape of the request that produced it. The audit trail is append-only: an
// intent that leaves these zero does not merely omit the provenance, it records
// an assurance of none and states that the operator did not assert an
// unarchived source — a false statement no later write can retract.
func (i *PricingAuditIntent) RecordSource(source PriceSource) {
	i.SourceType, i.SourceAssurance = source.Type, source.Assurance
	i.SourceReference, i.SourceContentSHA256 = source.Reference, source.ContentSHA256
	i.SourceWithoutArchive = source.AssertedWithoutArchive
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
	// Every action that establishes a price the gateway will bill against has to
	// say what backs it. A missing assurance is the signature of an intent built
	// from something other than the price source — the regression this guards is
	// exactly that, and it is unfixable once appended. cancel, proposal_reject,
	// restore_confirm and migrate act on prices that already carry their own
	// source and are deliberately not listed.
	switch i.Action {
	case "deployment_price.create", "deployment_price.proposal_create", "deployment_price.proposal_adopt":
		if strings.TrimSpace(string(i.SourceAssurance)) == "" {
			return errors.New("pricing audit intent is missing the source assurance for a price it establishes")
		}
		if strings.TrimSpace(string(i.SourceType)) == "" {
			return errors.New("pricing audit intent is missing the source type for a price it establishes")
		}
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
