package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	CachedInputMicrosPerMillion *int64 `json:"cached_input_micros_per_million,omitempty"`
	// CacheWriteMicrosPerMillion and CacheWriteLongMicrosPerMillion are the
	// one exception to "a missing term is refused": a snapshot written before
	// write rates existed has neither, and its attempt was settled — and is
	// replayed — without them, so they decode as absent and read as zero.
	// Nothing re-prices a settled attempt, which is what makes that safe; every
	// snapshot taken since carries both, and only an absent pair is accepted,
	// never one of the two.
	CacheWriteMicrosPerMillion     *int64 `json:"cache_write_micros_per_million,omitempty"`
	CacheWriteLongMicrosPerMillion *int64 `json:"cache_write_long_micros_per_million,omitempty"`
	OutputMicrosPerMillion         *int64 `json:"output_micros_per_million,omitempty"`
	FixedRequestMicrosUSD          *int64 `json:"fixed_request_micros_usd,omitempty"`
	// ScheduleTier names the rung of the price version's time-of-day schedule
	// that produced the billing terms above, and is nil exactly when the version
	// carries no schedule — so a fixed price's snapshot is byte-identical to
	// what it was before schedules existed, digest included.
	//
	// The terms themselves are already the selected rung's, which is why
	// nothing that reads a snapshot has to know about schedules to price
	// against one.
	ScheduleTier         *PriceScheduleTier   `json:"schedule_tier,omitempty"`
	EffectiveFrom        *time.Time           `json:"effective_from,omitempty"`
	SourceType           PriceSourceType      `json:"source_type,omitempty"`
	SourceAssurance      PriceSourceAssurance `json:"source_assurance,omitempty"`
	SourceContentSHA256  string               `json:"source_content_sha256,omitempty"`
	SourceReference      string               `json:"source_reference,omitempty"`
	SourceWithoutArchive bool                 `json:"source_without_archive,omitempty"`
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
	clone.CacheWriteMicrosPerMillion = cloneInt64(s.CacheWriteMicrosPerMillion)
	clone.CacheWriteLongMicrosPerMillion = cloneInt64(s.CacheWriteLongMicrosPerMillion)
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
	if s.ScheduleTier != nil {
		value := s.ScheduleTier.Clone()
		clone.ScheduleTier = &value
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
	// The rung is chosen here and nowhere else. Settlement re-reads the snapshot
	// rather than the clock, because the Ledger compares a settlement's snapshot
	// byte for byte with the reservation's: an attempt that starts before a
	// window boundary and finishes after it would otherwise settle against a
	// different rung and be rejected outright.
	tier := price.TierAt(selectedAt)
	version, effective := price.Version, price.EffectiveFrom
	input, cached := tier.InputMicrosPerMillion, tier.CachedInputMicrosPerMillion
	write, writeLong := tier.CacheWriteMicrosPerMillion, tier.CacheWriteLongMicrosPerMillion
	output, fixed := tier.OutputMicrosPerMillion, tier.FixedRequestMicrosUSD
	snapshot := PriceSnapshot{
		PricingSelectedAt: selectedAt, PriceEvidenceStatus: PriceEvidenceVersioned, CostValueStatus: CostValueKnown,
		PriceVersionID: price.ID, PriceVersion: &version, BillingMode: price.BillingMode,
		Currency: price.Currency, FormulaVersion: price.FormulaVersion,
		InputMicrosPerMillion: &input, CachedInputMicrosPerMillion: &cached,
		CacheWriteMicrosPerMillion: &write, CacheWriteLongMicrosPerMillion: &writeLong,
		OutputMicrosPerMillion: &output, FixedRequestMicrosUSD: &fixed, ScheduleTier: tier.Provenance,
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
		if (s.CacheWriteMicrosPerMillion == nil) != (s.CacheWriteLongMicrosPerMillion == nil) {
			return errors.New("versioned price snapshot carries only one cache write term")
		}
		if s.EffectiveFrom.IsZero() || !isUTC(*s.EffectiveFrom) || s.EffectiveFrom.After(s.PricingSelectedAt) {
			return errors.New("price snapshot effective_from is invalid")
		}
		if s.ScheduleTier != nil {
			if err := s.ScheduleTier.Validate(); err != nil {
				return err
			}
		}
		// Source-type-specific optional evidence is intentionally not reconstructed;
		// validate the immutable billing terms directly.
		rates := s.Tier()
		if rates.hasNegativeRate() {
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
			if rates.isZero() {
				return errors.New("metered snapshot requires a positive component")
			}
		case BillingModeFree:
			if !rates.isZero() {
				return errors.New("free snapshot components must be zero")
			}
		default:
			return errors.New("price snapshot billing mode is invalid")
		}
	case PriceEvidenceUnknown:
		if s.CostValueStatus != CostValueUnknown || s.PriceVersion != nil || s.PriceVersionID != "" ||
			s.InputMicrosPerMillion != nil || s.CachedInputMicrosPerMillion != nil ||
			s.CacheWriteMicrosPerMillion != nil || s.CacheWriteLongMicrosPerMillion != nil ||
			s.OutputMicrosPerMillion != nil || s.FixedRequestMicrosUSD != nil || s.ScheduleTier != nil ||
			s.SourceType != "" || s.SourceAssurance != "" || s.SourceContentSHA256 != "" || s.SourceReference != "" || s.SourceWithoutArchive {
			return errors.New("unknown price snapshot must not contain known price terms")
		}
	default:
		return fmt.Errorf("unsupported price evidence status %q", s.PriceEvidenceStatus)
	}
	return nil
}

// Calculate prices one attempt against this frozen snapshot.
func (s PriceSnapshot) Calculate(tokens BilledTokens) (PriceCostBreakdown, error) {
	if err := s.Validate(); err != nil {
		return PriceCostBreakdown{}, err
	}
	if s.CostValueStatus != CostValueKnown {
		return PriceCostBreakdown{}, ErrPriceUnavailable
	}
	if s.BillingMode == BillingModeFree {
		if err := tokens.Validate(); err != nil {
			return PriceCostBreakdown{}, err
		}
		return PriceCostBreakdown{}, nil
	}
	if s.CacheWriteMicrosPerMillion == nil {
		if err := tokens.Validate(); err != nil {
			return PriceCostBreakdown{}, err
		}
		// Pre-schema-43 snapshots billed writes within ordinary input.
		tokens.CacheWrite, tokens.CacheWriteLong = 0, 0
	}
	// The snapshot's terms are already the rung that was pinned at
	// reservation, whether or not the version behind it bills by time of day.
	// Pricing reads them; it never re-reads a clock.
	return s.Tier().Calculate(tokens)
}

// ReservationCost applies the conservative cost bound to the frozen tier.
func (s PriceSnapshot) ReservationCost(input, output int64) (int64, error) {
	if err := s.Validate(); err != nil {
		return 0, err
	}
	if s.CostValueStatus != CostValueKnown {
		return 0, ErrPriceUnavailable
	}
	if s.CacheWriteMicrosPerMillion == nil {
		cost, err := s.Tier().Calculate(s.ConservativeTokens(input, output))
		return cost.TotalCostMicrosUSD, err
	}
	return s.Tier().ReservationCost(input, output)
}

// ConservativeTokens is PriceTier.ConservativeTokens against the frozen rung.
// A snapshot with unknown cost has no rates, and every prompt token is then
// ordinary input.
func (s PriceSnapshot) ConservativeTokens(inputTokens, outputTokens int64) BilledTokens {
	if s.CacheWriteMicrosPerMillion == nil {
		// Recover an old reservation using its original all-ordinary estimate.
		return BilledTokens{Input: inputTokens, Output: outputTokens}
	}
	return s.Tier().ConservativeTokens(inputTokens, outputTokens)
}

// Tier is the frozen rung's rates. A term the snapshot does not carry reads
// as zero — only the cache-write pair of a snapshot written before it existed
// can be absent on a snapshot that validates.
func (s PriceSnapshot) Tier() PriceTier {
	return PriceTier{
		InputMicrosPerMillion:          derefInt64(s.InputMicrosPerMillion),
		CachedInputMicrosPerMillion:    derefInt64(s.CachedInputMicrosPerMillion),
		CacheWriteMicrosPerMillion:     derefInt64(s.CacheWriteMicrosPerMillion),
		CacheWriteLongMicrosPerMillion: derefInt64(s.CacheWriteLongMicrosPerMillion),
		OutputMicrosPerMillion:         derefInt64(s.OutputMicrosPerMillion),
		FixedRequestMicrosUSD:          derefInt64(s.FixedRequestMicrosUSD),
		Provenance:                     s.ScheduleTier,
	}
}

func derefInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
