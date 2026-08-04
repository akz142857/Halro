package domain

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

type BillingMode string

const (
	BillingModeMetered BillingMode = "metered"
	BillingModeFree    BillingMode = "free"
)

type PriceFormulaVersion string

const PriceFormulaUSDTokensV1 PriceFormulaVersion = "usd_token_v1"

type PriceEvidenceStatus string

const (
	PriceEvidenceVersioned         PriceEvidenceStatus = "versioned"
	PriceEvidenceUnknown           PriceEvidenceStatus = "unknown"
	PriceEvidenceLegacyUnversioned PriceEvidenceStatus = "legacy_unversioned"
)

type CostValueStatus string

const (
	CostValueKnown   CostValueStatus = "known"
	CostValueUnknown CostValueStatus = "unknown"
)

type PriceSourceType string

const (
	PriceSourceManual      PriceSourceType = "manual"
	PriceSourceOfficialURL PriceSourceType = "official_url"
	PriceSourceProviderAPI PriceSourceType = "provider_api"
	PriceSourceImport      PriceSourceType = "import"
	PriceSourceMigration   PriceSourceType = "migration"
)

type PriceSourceAssurance string

const (
	PriceAssuranceAsserted     PriceSourceAssurance = "asserted"
	PriceAssuranceVerifiedAPI  PriceSourceAssurance = "verified_api"
	PriceAssuranceSignedImport PriceSourceAssurance = "signed_import"
)

type PriceLifecycleStatus string

const (
	PriceLifecycleScheduled  PriceLifecycleStatus = "scheduled"
	PriceLifecycleActive     PriceLifecycleStatus = "active"
	PriceLifecycleSuperseded PriceLifecycleStatus = "superseded"
	PriceLifecycleCancelled  PriceLifecycleStatus = "cancelled"
)

var (
	ErrPriceUnavailable        = errors.New("price unavailable")
	ErrPriceTimelineConflict   = errors.New("price timeline conflict")
	ErrPriceVersionUnavailable = errors.New("price version unavailable")
)

var pricingSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[a-z0-9._~+/=-]{12,}`),
	regexp.MustCompile(`\b(?:sk|pk|ghp|github_pat|xox[baprs])-[_A-Za-z0-9]{12,}`),
	regexp.MustCompile(`\bAKIA[A-Z0-9]{16}\b`),
	regexp.MustCompile(`(?i)(?:api[_-]?key|access[_-]?token|client[_-]?secret|password)\s*[:=]\s*[^\s&]{8,}`),
}

// PriceSource stores evidence metadata, never the source document itself.
type PriceSource struct {
	Type                   PriceSourceType      `json:"type"`
	Assurance              PriceSourceAssurance `json:"assurance"`
	URI                    string               `json:"uri,omitempty"`
	PublishedAt            *time.Time           `json:"published_at,omitempty"`
	RetrievedAt            *time.Time           `json:"retrieved_at,omitempty"`
	ReceivedAt             time.Time            `json:"received_at"`
	ContentSHA256          string               `json:"content_sha256"`
	Reference              string               `json:"reference,omitempty"`
	Note                   string               `json:"note,omitempty"`
	ExternalEvidenceID     string               `json:"external_evidence_id,omitempty"`
	CustodyOwner           string               `json:"custody_owner,omitempty"`
	AssertedWithoutArchive bool                 `json:"asserted_without_archive,omitempty"`
	Adapter                string               `json:"adapter,omitempty"`
	ProviderRequestID      string               `json:"provider_request_id,omitempty"`
	ManifestID             string               `json:"manifest_id,omitempty"`
	ManifestSchemaVersion  uint64               `json:"manifest_schema_version,omitempty"`
	Signer                 string               `json:"signer,omitempty"`
	SignatureSHA256        string               `json:"signature_sha256,omitempty"`
	MigrationVersion       uint64               `json:"migration_version,omitempty"`
	OriginalResourceID     string               `json:"original_resource_id,omitempty"`
	OriginalRevision       uint64               `json:"original_revision,omitempty"`
}

func (s PriceSource) Validate() error {
	var problems []error
	if s.ReceivedAt.IsZero() {
		problems = append(problems, errors.New("price source received_at is required"))
	} else if !isUTC(s.ReceivedAt) {
		problems = append(problems, errors.New("price source received_at must use UTC"))
	}
	if !validSHA256Label(s.ContentSHA256) {
		problems = append(problems, errors.New("price source content_sha256 is invalid"))
	}
	for label, item := range map[string]struct {
		value string
		max   int
	}{
		"uri": {s.URI, 2048}, "reference": {s.Reference, 256}, "note": {s.Note, 1024},
		"external evidence id": {s.ExternalEvidenceID, 256}, "custody owner": {s.CustodyOwner, 256},
		"adapter": {s.Adapter, 128}, "provider request id": {s.ProviderRequestID, 256},
		"manifest id": {s.ManifestID, 256}, "signer": {s.Signer, 256},
		"original resource id": {s.OriginalResourceID, 256},
	} {
		if len(item.value) > item.max {
			problems = append(problems, fmt.Errorf("price source %s exceeds %d bytes", label, item.max))
		}
		for _, pattern := range pricingSecretPatterns {
			if pattern.MatchString(item.value) {
				problems = append(problems, fmt.Errorf("price source %s contains credential-like secret material", label))
				break
			}
		}
	}

	switch s.Type {
	case PriceSourceManual:
		if s.Assurance != PriceAssuranceAsserted {
			problems = append(problems, errors.New("manual price source assurance must be asserted"))
		}
		if strings.TrimSpace(s.Reference) == "" ||
			(strings.TrimSpace(s.Note) == "" && strings.TrimSpace(s.ExternalEvidenceID) == "" && !s.AssertedWithoutArchive) {
			problems = append(problems, errors.New("manual price source requires reference and evidence or asserted_without_archive"))
		}
	case PriceSourceOfficialURL:
		if s.Assurance != PriceAssuranceAsserted {
			problems = append(problems, errors.New("official_url price source assurance must be asserted"))
		}
		if strings.TrimSpace(s.Reference) == "" || s.RetrievedAt == nil {
			problems = append(problems, errors.New("official_url price source requires reference and retrieved_at"))
		}
		if err := validateEvidenceURL(s.URI); err != nil {
			problems = append(problems, err)
		}
	case PriceSourceProviderAPI:
		if s.Assurance != PriceAssuranceVerifiedAPI || strings.TrimSpace(s.Adapter) == "" || strings.TrimSpace(s.ProviderRequestID) == "" {
			problems = append(problems, errors.New("provider_api source requires verified_api assurance, adapter, and provider request id"))
		}
	case PriceSourceImport:
		if s.Assurance != PriceAssuranceSignedImport || strings.TrimSpace(s.ManifestID) == "" ||
			s.ManifestSchemaVersion == 0 || strings.TrimSpace(s.Signer) == "" || !validSHA256Label(s.SignatureSHA256) {
			problems = append(problems, errors.New("import source requires signed manifest metadata"))
		}
	case PriceSourceMigration:
		if s.Assurance != PriceAssuranceAsserted || s.MigrationVersion == 0 ||
			strings.TrimSpace(s.OriginalResourceID) == "" || s.OriginalRevision == 0 {
			problems = append(problems, errors.New("migration source requires asserted assurance and origin metadata"))
		}
	default:
		problems = append(problems, errors.New("price source type is invalid"))
	}
	for label, value := range map[string]*time.Time{"published_at": s.PublishedAt, "retrieved_at": s.RetrievedAt} {
		if value != nil && (!isUTC(*value) || value.IsZero()) {
			problems = append(problems, fmt.Errorf("price source %s must be a non-zero UTC time", label))
		}
	}
	return errors.Join(problems...)
}

// DeploymentPriceVersion is immutable except for its cancellation metadata and
// revision. Lifecycle status is derived from the timeline at query time.
type DeploymentPriceVersion struct {
	ID                     string              `json:"id"`
	DeploymentID           string              `json:"deployment_id"`
	Version                uint64              `json:"version"`
	BillingMode            BillingMode         `json:"billing_mode"`
	Currency               string              `json:"currency"`
	FormulaVersion         PriceFormulaVersion `json:"formula_version"`
	InputMicrosPerMillion  int64               `json:"input_micros_per_million"`
	OutputMicrosPerMillion int64               `json:"output_micros_per_million"`
	FixedRequestMicrosUSD  int64               `json:"fixed_request_micros_usd"`
	EffectiveFrom          time.Time           `json:"effective_from"`
	Source                 PriceSource         `json:"source"`
	CreatedBy              string              `json:"created_by"`
	CreatedAt              time.Time           `json:"created_at"`
	CancelledBy            string              `json:"cancelled_by,omitempty"`
	CancelledAt            *time.Time          `json:"cancelled_at,omitempty"`
	Revision               uint64              `json:"revision"`
}

func (p *DeploymentPriceVersion) GetRevision() uint64      { return p.Revision }
func (p *DeploymentPriceVersion) SetRevision(value uint64) { p.Revision = value }

func (p DeploymentPriceVersion) Validate() error {
	var problems []error
	if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.DeploymentID) == "" {
		problems = append(problems, errors.New("price version id and deployment id are required"))
	}
	if p.Version == 0 || p.Revision == 0 {
		problems = append(problems, errors.New("price version and revision must be positive"))
	}
	if p.Currency != "USD" {
		problems = append(problems, errors.New("price currency must be USD"))
	}
	if p.FormulaVersion != PriceFormulaUSDTokensV1 {
		problems = append(problems, errors.New("price formula version is unsupported"))
	}
	if p.InputMicrosPerMillion < 0 || p.OutputMicrosPerMillion < 0 || p.FixedRequestMicrosUSD < 0 {
		problems = append(problems, errors.New("price amounts cannot be negative"))
	}
	switch p.BillingMode {
	case BillingModeMetered:
		if p.InputMicrosPerMillion == 0 && p.OutputMicrosPerMillion == 0 && p.FixedRequestMicrosUSD == 0 {
			problems = append(problems, errors.New("metered price requires a positive component"))
		}
	case BillingModeFree:
		if p.InputMicrosPerMillion != 0 || p.OutputMicrosPerMillion != 0 || p.FixedRequestMicrosUSD != 0 {
			problems = append(problems, errors.New("free price components must all be zero"))
		}
	default:
		problems = append(problems, errors.New("price billing mode is invalid"))
	}
	if p.EffectiveFrom.IsZero() || !isUTC(p.EffectiveFrom) {
		problems = append(problems, errors.New("price effective_from must be a non-zero UTC time"))
	}
	if strings.TrimSpace(p.CreatedBy) == "" || p.CreatedAt.IsZero() || !isUTC(p.CreatedAt) {
		problems = append(problems, errors.New("price creator and UTC created_at are required"))
	}
	if (p.CancelledAt == nil) != (strings.TrimSpace(p.CancelledBy) == "") {
		problems = append(problems, errors.New("price cancellation actor and time must be set together"))
	}
	if p.CancelledAt != nil {
		if p.CancelledAt.IsZero() || !isUTC(*p.CancelledAt) || !p.CancelledAt.Before(p.EffectiveFrom) {
			problems = append(problems, errors.New("price must be cancelled in UTC before effective_from"))
		}
	}
	if err := p.Source.Validate(); err != nil {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}

func SelectDeploymentPriceVersion(versions []DeploymentPriceVersion, deploymentID string, selectedAt time.Time) (DeploymentPriceVersion, error) {
	if strings.TrimSpace(deploymentID) == "" || selectedAt.IsZero() || !isUTC(selectedAt) {
		return DeploymentPriceVersion{}, errors.New("deployment id and UTC pricing_selected_at are required")
	}
	var selected *DeploymentPriceVersion
	seenEffective := make(map[[12]byte]string)
	seenVersion := make(map[uint64]string)
	for index := range versions {
		candidate := versions[index]
		if err := candidate.Validate(); err != nil {
			return DeploymentPriceVersion{}, fmt.Errorf("price version %q: %w", candidate.ID, err)
		}
		if candidate.DeploymentID != deploymentID {
			return DeploymentPriceVersion{}, fmt.Errorf("price version %q belongs to another deployment", candidate.ID)
		}
		timeKey := canonicalPriceTime(candidate.EffectiveFrom)
		if previous, exists := seenEffective[timeKey]; exists && previous != candidate.ID {
			return DeploymentPriceVersion{}, fmt.Errorf("%w: duplicate effective time", ErrPriceTimelineConflict)
		}
		seenEffective[timeKey] = candidate.ID
		if previous, exists := seenVersion[candidate.Version]; exists && previous != candidate.ID {
			return DeploymentPriceVersion{}, fmt.Errorf("%w: duplicate version", ErrPriceTimelineConflict)
		}
		seenVersion[candidate.Version] = candidate.ID
		if candidate.CancelledAt != nil || candidate.EffectiveFrom.After(selectedAt) {
			continue
		}
		if selected == nil || candidate.EffectiveFrom.After(selected.EffectiveFrom) ||
			(candidate.EffectiveFrom.Equal(selected.EffectiveFrom) && (candidate.Version > selected.Version ||
				(candidate.Version == selected.Version && candidate.ID > selected.ID))) {
			copy := candidate
			selected = &copy
		}
	}
	if selected == nil {
		return DeploymentPriceVersion{}, ErrPriceUnavailable
	}
	return *selected, nil
}

func DerivePriceLifecycle(versions []DeploymentPriceVersion, deploymentID string, selectedAt time.Time) (map[string]PriceLifecycleStatus, error) {
	result := make(map[string]PriceLifecycleStatus, len(versions))
	selected, selectErr := SelectDeploymentPriceVersion(versions, deploymentID, selectedAt)
	if selectErr != nil && !errors.Is(selectErr, ErrPriceUnavailable) {
		return nil, selectErr
	}
	ordered := append([]DeploymentPriceVersion(nil), versions...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].EffectiveFrom.Equal(ordered[j].EffectiveFrom) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].EffectiveFrom.Before(ordered[j].EffectiveFrom)
	})
	for _, candidate := range ordered {
		if candidate.CancelledAt != nil {
			result[candidate.ID] = PriceLifecycleCancelled
		} else if candidate.EffectiveFrom.After(selectedAt) {
			result[candidate.ID] = PriceLifecycleScheduled
		} else if selectErr == nil && candidate.ID == selected.ID {
			result[candidate.ID] = PriceLifecycleActive
		} else {
			result[candidate.ID] = PriceLifecycleSuperseded
		}
	}
	return result, nil
}

func validateEvidenceURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("official price source URI must be HTTPS without userinfo, query, or fragment")
	}
	return nil
}

func validSHA256Label(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func ValidSHA256Label(value string) bool { return validSHA256Label(value) }

func isUTC(value time.Time) bool {
	_, offset := value.Zone()
	return offset == 0
}

func canonicalPriceTime(value time.Time) [12]byte {
	var encoded [12]byte
	utc := value.UTC()
	binary.BigEndian.PutUint64(encoded[:8], uint64(utc.Unix())^(uint64(1)<<63))
	binary.BigEndian.PutUint32(encoded[8:], uint32(utc.Nanosecond()))
	return encoded
}
