package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type PriceProposalMatch string

const (
	PriceProposalMatchExact     PriceProposalMatch = "exact"
	PriceProposalMatchLikely    PriceProposalMatch = "likely"
	PriceProposalMatchAmbiguous PriceProposalMatch = "ambiguous"
)

type PriceProposalStatus string

const (
	PriceProposalPending  PriceProposalStatus = "pending"
	PriceProposalAdopted  PriceProposalStatus = "adopted"
	PriceProposalRejected PriceProposalStatus = "rejected"
)

// DeploymentPriceProposal is evidence for human review. It is never consulted
// by price selection or the Gateway request path.
type DeploymentPriceProposal struct {
	ID                    string              `json:"id"`
	DeploymentID          string              `json:"deployment_id"`
	ProviderID            string              `json:"provider_id"`
	ProviderModel         string              `json:"provider_model"`
	Region                string              `json:"region,omitempty"`
	Tier                  string              `json:"tier,omitempty"`
	BillingMode           BillingMode         `json:"billing_mode"`
	Currency              string              `json:"currency"`
	FormulaVersion        PriceFormulaVersion `json:"formula_version"`
	InputMicrosPerMillion int64               `json:"input_micros_per_million"`
	// CachedInputMicrosPerMillion mirrors the adopted price version's cache-read
	// term, so adopting a proposal produces a complete price rather than one
	// that silently prices cached prompt tokens at zero.
	CachedInputMicrosPerMillion int64               `json:"cached_input_micros_per_million"`
	OutputMicrosPerMillion      int64               `json:"output_micros_per_million"`
	FixedRequestMicrosUSD       int64               `json:"fixed_request_micros_usd"`
	Source                      PriceSource         `json:"source"`
	FetchedAt                   time.Time           `json:"fetched_at"`
	Warnings                    []string            `json:"warnings,omitempty"`
	Match                       PriceProposalMatch  `json:"match"`
	ExpiresAt                   time.Time           `json:"expires_at"`
	Digest                      string              `json:"digest"`
	Status                      PriceProposalStatus `json:"status"`
	CreatedBy                   string              `json:"created_by"`
	CreatedAt                   time.Time           `json:"created_at"`
	AdoptedPriceVersionID       string              `json:"adopted_price_version_id,omitempty"`
	ReviewedBy                  string              `json:"reviewed_by,omitempty"`
	ReviewedAt                  *time.Time          `json:"reviewed_at,omitempty"`
	Revision                    uint64              `json:"revision"`
}

func (p DeploymentPriceProposal) Validate() error {
	if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.DeploymentID) == "" ||
		strings.TrimSpace(p.ProviderID) == "" || strings.TrimSpace(p.ProviderModel) == "" ||
		strings.TrimSpace(p.CreatedBy) == "" || p.Revision == 0 || !validSHA256Label(p.Digest) {
		return errors.New("invalid price proposal identity")
	}
	if len(p.ProviderModel) > 256 || len(p.Region) > 128 || len(p.Tier) > 128 || len(p.Warnings) > 32 {
		return errors.New("price proposal field exceeds maximum size")
	}
	for _, warning := range p.Warnings {
		if strings.TrimSpace(warning) == "" || len(warning) > 512 {
			return errors.New("invalid price proposal warning")
		}
	}
	if p.Currency != "USD" || p.FormulaVersion != PriceFormulaUSDTokensV1 ||
		p.InputMicrosPerMillion < 0 || p.CachedInputMicrosPerMillion < 0 || p.OutputMicrosPerMillion < 0 || p.FixedRequestMicrosUSD < 0 {
		return errors.New("invalid price proposal candidate")
	}
	if p.BillingMode == BillingModeMetered && p.InputMicrosPerMillion == 0 && p.CachedInputMicrosPerMillion == 0 &&
		p.OutputMicrosPerMillion == 0 && p.FixedRequestMicrosUSD == 0 {
		return errors.New("metered proposal requires a positive component")
	}
	if p.BillingMode == BillingModeFree && (p.InputMicrosPerMillion != 0 || p.CachedInputMicrosPerMillion != 0 ||
		p.OutputMicrosPerMillion != 0 || p.FixedRequestMicrosUSD != 0) {
		return errors.New("free proposal components must be zero")
	}
	if p.BillingMode != BillingModeMetered && p.BillingMode != BillingModeFree {
		return errors.New("invalid price proposal billing mode")
	}
	if p.Match != PriceProposalMatchExact && p.Match != PriceProposalMatchLikely && p.Match != PriceProposalMatchAmbiguous {
		return errors.New("invalid price proposal match status")
	}
	if p.FetchedAt.IsZero() || p.ExpiresAt.IsZero() || p.CreatedAt.IsZero() || !isUTC(p.FetchedAt) || !isUTC(p.ExpiresAt) || !isUTC(p.CreatedAt) || !p.ExpiresAt.After(p.FetchedAt) {
		return errors.New("invalid price proposal timestamps")
	}
	if err := p.Source.Validate(); err != nil {
		return err
	}
	if p.Source.Type != PriceSourceOfficialURL && p.Source.Type != PriceSourceProviderAPI && p.Source.Type != PriceSourceImport {
		return errors.New("price proposal requires an official, provider API, or signed import source")
	}
	if p.Status != PriceProposalPending && p.Status != PriceProposalAdopted && p.Status != PriceProposalRejected {
		return errors.New("invalid price proposal status")
	}
	if p.Status == PriceProposalPending {
		if p.ReviewedAt != nil || p.ReviewedBy != "" || p.AdoptedPriceVersionID != "" {
			return errors.New("pending price proposal cannot contain review outcome")
		}
	} else if p.ReviewedAt == nil || !isUTC(*p.ReviewedAt) || strings.TrimSpace(p.ReviewedBy) == "" ||
		(p.Status == PriceProposalAdopted) != (p.AdoptedPriceVersionID != "") {
		return errors.New("reviewed price proposal outcome is incomplete")
	}
	expected, err := p.ComputeDigest()
	if err != nil || expected != p.Digest {
		return errors.New("price proposal digest mismatch")
	}
	return nil
}

func (p DeploymentPriceProposal) ComputeDigest() (string, error) {
	p.Digest, p.Status, p.CreatedBy, p.CreatedAt, p.AdoptedPriceVersionID, p.ReviewedBy, p.ReviewedAt, p.Revision = "", "", "", time.Time{}, "", "", nil, 0
	p.ID = ""
	p.Source.ReceivedAt = time.Time{}
	encoded, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}
