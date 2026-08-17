package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// PriceSnapshot is the self-contained, immutable pricing evidence captured for
// one provider attempt. Pointer-valued terms preserve the distinction between
// a known zero-cost free price and an unknown price.
type PriceSnapshot struct {
	PricingSelectedAt     time.Time           `json:"pricing_selected_at"`
	PriceEvidenceStatus   PriceEvidenceStatus `json:"price_evidence_status"`
	CostValueStatus       CostValueStatus     `json:"cost_value_status"`
	PriceVersionID        string              `json:"price_version_id,omitempty"`
	PriceVersion          *uint64             `json:"price_version,omitempty"`
	BillingMode           BillingMode         `json:"billing_mode,omitempty"`
	Currency              string              `json:"currency,omitempty"`
	FormulaVersion        PriceFormulaVersion `json:"formula_version,omitempty"`
	InputMicrosPerMillion *int64              `json:"input_micros_per_million,omitempty"`
	// CachedInputMicrosPerMillion is required on a versioned snapshot. A missing
	// term would price the cached span at zero, so a snapshot that lacks it is
	// refused rather than read as free — the same fail-closed rule the other
	// billing terms already follow.
	CachedInputMicrosPerMillion *int64               `json:"cached_input_micros_per_million,omitempty"`
	OutputMicrosPerMillion      *int64               `json:"output_micros_per_million,omitempty"`
	FixedRequestMicrosUSD       *int64               `json:"fixed_request_micros_usd,omitempty"`
	EffectiveFrom               *time.Time           `json:"effective_from,omitempty"`
	SourceType                  PriceSourceType      `json:"source_type,omitempty"`
	SourceAssurance             PriceSourceAssurance `json:"source_assurance,omitempty"`
	SourceContentSHA256         string               `json:"source_content_sha256,omitempty"`
	SourceReference             string               `json:"source_reference,omitempty"`
	SourceWithoutArchive        bool                 `json:"source_without_archive,omitempty"`
}

// Clone returns a deep copy suitable for crossing an accounting/WAL boundary.
func (s PriceSnapshot) Clone() PriceSnapshot {
	clone := s
	if s.PriceVersion != nil {
		value := *s.PriceVersion
		clone.PriceVersion = &value
	}
	if s.InputMicrosPerMillion != nil {
		value := *s.InputMicrosPerMillion
		clone.InputMicrosPerMillion = &value
	}
	if s.CachedInputMicrosPerMillion != nil {
		value := *s.CachedInputMicrosPerMillion
		clone.CachedInputMicrosPerMillion = &value
	}
	if s.OutputMicrosPerMillion != nil {
		value := *s.OutputMicrosPerMillion
		clone.OutputMicrosPerMillion = &value
	}
	if s.FixedRequestMicrosUSD != nil {
		value := *s.FixedRequestMicrosUSD
		clone.FixedRequestMicrosUSD = &value
	}
	if s.EffectiveFrom != nil {
		value := *s.EffectiveFrom
		clone.EffectiveFrom = &value
	}
	return clone
}

type UnknownPricePolicyEvidence struct {
	PolicyVersion          string `json:"policy_version"`
	ProjectID              string `json:"project_id"`
	TokenGuardStatus       string `json:"token_guard_status"`
	ReasonCode             string `json:"reason_code"`
	InstanceExplicitOptIn  bool   `json:"instance_explicit_opt_in"`
	CostGovernanceDisabled bool   `json:"cost_governance_disabled"`
}

func (e UnknownPricePolicyEvidence) Validate() error {
	if e.PolicyVersion == "" || e.ProjectID == "" || e.TokenGuardStatus == "" || e.ReasonCode != "cost_governance_disabled" ||
		!e.InstanceExplicitOptIn || !e.CostGovernanceDisabled {
		return errors.New("unknown price policy evidence is incomplete or does not explicitly disable cost governance")
	}
	return nil
}

func (s PriceSnapshot) Digest() (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func NewVersionedPriceSnapshot(price DeploymentPriceVersion, selectedAt time.Time) (PriceSnapshot, error) {
	if err := price.Validate(); err != nil {
		return PriceSnapshot{}, err
	}
	selectedAt = selectedAt.UTC()
	version, input, output, fixed, effective := price.Version, price.InputMicrosPerMillion, price.OutputMicrosPerMillion, price.FixedRequestMicrosUSD, price.EffectiveFrom
	cached := price.CachedInputMicrosPerMillion
	snapshot := PriceSnapshot{
		PricingSelectedAt: selectedAt, PriceEvidenceStatus: PriceEvidenceVersioned, CostValueStatus: CostValueKnown,
		PriceVersionID: price.ID, PriceVersion: &version, BillingMode: price.BillingMode,
		Currency: price.Currency, FormulaVersion: price.FormulaVersion,
		InputMicrosPerMillion: &input, CachedInputMicrosPerMillion: &cached,
		OutputMicrosPerMillion: &output, FixedRequestMicrosUSD: &fixed,
		EffectiveFrom: &effective, SourceType: price.Source.Type, SourceAssurance: price.Source.Assurance,
		SourceContentSHA256: price.Source.ContentSHA256, SourceReference: price.Source.Reference,
		SourceWithoutArchive: price.Source.AssertedWithoutArchive,
	}
	return snapshot, snapshot.Validate()
}

func NewUnknownPriceSnapshot(selectedAt time.Time) PriceSnapshot {
	return PriceSnapshot{PricingSelectedAt: selectedAt.UTC(), PriceEvidenceStatus: PriceEvidenceUnknown, CostValueStatus: CostValueUnknown}
}

func (s PriceSnapshot) Validate() error {
	if s.PricingSelectedAt.IsZero() || !isUTC(s.PricingSelectedAt) {
		return errors.New("price snapshot pricing_selected_at must be non-zero UTC")
	}
	switch s.PriceEvidenceStatus {
	case PriceEvidenceVersioned:
		if s.CostValueStatus != CostValueKnown || s.PriceVersionID == "" || s.PriceVersion == nil || *s.PriceVersion == 0 ||
			s.EffectiveFrom == nil || s.InputMicrosPerMillion == nil || s.CachedInputMicrosPerMillion == nil ||
			s.OutputMicrosPerMillion == nil || s.FixedRequestMicrosUSD == nil {
			return errors.New("versioned price snapshot is incomplete")
		}
		if s.EffectiveFrom.IsZero() || !isUTC(*s.EffectiveFrom) || s.EffectiveFrom.After(s.PricingSelectedAt) {
			return errors.New("price snapshot effective_from is invalid")
		}
		price := DeploymentPriceVersion{
			ID: s.PriceVersionID, DeploymentID: "snapshot", Version: *s.PriceVersion, Revision: 1,
			BillingMode: s.BillingMode, Currency: s.Currency, FormulaVersion: s.FormulaVersion,
			InputMicrosPerMillion: *s.InputMicrosPerMillion, CachedInputMicrosPerMillion: *s.CachedInputMicrosPerMillion,
			OutputMicrosPerMillion: *s.OutputMicrosPerMillion,
			FixedRequestMicrosUSD:  *s.FixedRequestMicrosUSD, EffectiveFrom: *s.EffectiveFrom,
			CreatedBy: "snapshot", CreatedAt: s.PricingSelectedAt,
			Source: PriceSource{Type: s.SourceType, Assurance: s.SourceAssurance, ReceivedAt: s.PricingSelectedAt,
				ContentSHA256: s.SourceContentSHA256, Reference: "snapshot", AssertedWithoutArchive: true},
		}
		// Source-type-specific optional evidence is intentionally not reconstructed;
		// validate the immutable billing terms directly.
		if price.InputMicrosPerMillion < 0 || price.CachedInputMicrosPerMillion < 0 ||
			price.OutputMicrosPerMillion < 0 || price.FixedRequestMicrosUSD < 0 {
			return errors.New("price snapshot amounts cannot be negative")
		}
		manualWithoutArchive := s.SourceType == PriceSourceManual && s.SourceAssurance == PriceAssuranceAsserted &&
			s.SourceWithoutArchive && s.SourceContentSHA256 == "" && strings.TrimSpace(s.SourceReference) != ""
		validSourceIdentity := (s.SourceType == PriceSourceManual && s.SourceAssurance == PriceAssuranceAsserted) ||
			(s.SourceType == PriceSourceOfficialURL && s.SourceAssurance == PriceAssuranceAsserted) ||
			(s.SourceType == PriceSourceProviderAPI && s.SourceAssurance == PriceAssuranceVerifiedAPI) ||
			(s.SourceType == PriceSourceImport && s.SourceAssurance == PriceAssuranceSignedImport) ||
			(s.SourceType == PriceSourceMigration && s.SourceAssurance == PriceAssuranceAsserted)
		if s.Currency != "USD" || s.FormulaVersion != PriceFormulaUSDTokensV1 || !validSourceIdentity || len(s.SourceReference) > 256 ||
			((s.SourceType == PriceSourceManual || s.SourceType == PriceSourceOfficialURL) && strings.TrimSpace(s.SourceReference) == "") ||
			(!manualWithoutArchive && !validSHA256Label(s.SourceContentSHA256)) {
			return errors.New("price snapshot currency, formula, or evidence digest is invalid")
		}
		switch s.BillingMode {
		case BillingModeMetered:
			if *s.InputMicrosPerMillion == 0 && *s.CachedInputMicrosPerMillion == 0 &&
				*s.OutputMicrosPerMillion == 0 && *s.FixedRequestMicrosUSD == 0 {
				return errors.New("metered snapshot requires a positive component")
			}
		case BillingModeFree:
			if *s.InputMicrosPerMillion != 0 || *s.CachedInputMicrosPerMillion != 0 ||
				*s.OutputMicrosPerMillion != 0 || *s.FixedRequestMicrosUSD != 0 {
				return errors.New("free snapshot components must be zero")
			}
		default:
			return errors.New("price snapshot billing mode is invalid")
		}
	case PriceEvidenceUnknown:
		if s.CostValueStatus != CostValueUnknown || s.PriceVersion != nil || s.PriceVersionID != "" ||
			s.InputMicrosPerMillion != nil || s.CachedInputMicrosPerMillion != nil ||
			s.OutputMicrosPerMillion != nil || s.FixedRequestMicrosUSD != nil ||
			s.SourceType != "" || s.SourceAssurance != "" || s.SourceContentSHA256 != "" || s.SourceReference != "" || s.SourceWithoutArchive {
			return errors.New("unknown price snapshot must not contain known price terms")
		}
	default:
		return fmt.Errorf("unsupported price evidence status %q", s.PriceEvidenceStatus)
	}
	return nil
}

// Calculate prices one attempt against this frozen snapshot. cachedInputTokens
// is the subset of inputTokens the provider served from cache; callers that
// cannot know it yet pass zero and are charged the ordinary input rate for the
// whole prompt.
func (s PriceSnapshot) Calculate(inputTokens, cachedInputTokens, outputTokens int64) (PriceCostBreakdown, error) {
	if err := s.Validate(); err != nil {
		return PriceCostBreakdown{}, err
	}
	if s.CostValueStatus != CostValueKnown {
		return PriceCostBreakdown{}, ErrPriceUnavailable
	}
	if inputTokens < 0 || cachedInputTokens < 0 || outputTokens < 0 {
		return PriceCostBreakdown{}, errors.New("token counts cannot be negative")
	}
	if cachedInputTokens > inputTokens {
		return PriceCostBreakdown{}, errors.New("cached input tokens cannot exceed input tokens")
	}
	if s.BillingMode == BillingModeFree {
		return PriceCostBreakdown{}, nil
	}
	input, err := ceilInputComponents(inputTokens, cachedInputTokens, *s.InputMicrosPerMillion, *s.CachedInputMicrosPerMillion)
	if err != nil {
		return PriceCostBreakdown{}, err
	}
	output, err := ceilTokenComponent(outputTokens, *s.OutputMicrosPerMillion)
	if err != nil {
		return PriceCostBreakdown{}, err
	}
	total := new(big.Int).SetInt64(input)
	total.Add(total, big.NewInt(output))
	total.Add(total, big.NewInt(*s.FixedRequestMicrosUSD))
	if !total.IsInt64() || total.Sign() < 0 {
		return PriceCostBreakdown{}, errors.New("price calculation overflows int64 micro-USD")
	}
	return PriceCostBreakdown{InputCostMicrosUSD: input, OutputCostMicrosUSD: output,
		FixedCostMicrosUSD: *s.FixedRequestMicrosUSD, TotalCostMicrosUSD: total.Int64()}, nil
}
