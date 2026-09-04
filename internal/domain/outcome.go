package domain

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxActiveOutcomeDefinitions = 64
	MaxDefinitionsPerWorkUnit   = 8
	MaxOutcomeRevisions         = 20
	OutcomeWriteWindow          = 30 * 24 * time.Hour
)

type OutcomeDataType string

const (
	OutcomeBoolean     OutcomeDataType = "BOOLEAN"
	OutcomeCategorical OutcomeDataType = "CATEGORICAL"
)

type OutcomeDefinitionRef struct {
	ID      string `json:"id"`
	Version uint64 `json:"version"`
}

type OutcomeDefinition struct {
	ID            string          `json:"id"`
	ProjectID     string          `json:"project_id"`
	Name          string          `json:"name"`
	Version       uint64          `json:"version"`
	DataType      OutcomeDataType `json:"data_type"`
	AllowedValues []string        `json:"allowed_values"`
	SuccessValues []string        `json:"success_values"`
	Unit          string          `json:"unit,omitempty"`
	Description   string          `json:"description,omitempty"`
	Enabled       bool            `json:"enabled"`
	CreatedAt     time.Time       `json:"created_at"`
	CreatedBy     string          `json:"created_by"`
	Revision      uint64          `json:"revision"`
}

func ValidOutcomeDefinitionID(value string) bool { return validGovernanceID(value, "odef_") }
func ValidOutcomeID(value string) bool           { return validGovernanceID(value, "out_") }

var outcomeNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var outcomeValuePattern = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,32}$`)
var rawSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func ValidEvidenceSHA256(value string) bool { return rawSHA256Pattern.MatchString(value) }

func (d OutcomeDefinition) Validate() error {
	if !ValidOutcomeDefinitionID(d.ID) || d.ProjectID == "" || d.Version == 0 ||
		!outcomeNamePattern.MatchString(d.Name) || d.CreatedAt.IsZero() || d.CreatedBy == "" {
		return errors.New("outcome definition identity is invalid")
	}
	if utf8.RuneCountInString(d.Description) > 256 || len(d.Unit) > 32 {
		return errors.New("outcome definition text exceeds maximum size")
	}
	allowed := d.AllowedValues
	switch d.DataType {
	case OutcomeBoolean:
		if len(allowed) == 0 {
			allowed = []string{"false", "true"}
		}
		if !slices.Equal(allowed, []string{"false", "true"}) {
			return errors.New("BOOLEAN allowed_values must be false,true")
		}
	case OutcomeCategorical:
		if len(allowed) < 2 || len(allowed) > 16 {
			return errors.New("CATEGORICAL allowed_values must contain 2 to 16 values")
		}
	default:
		return errors.New("outcome definition data_type is invalid")
	}
	seen := map[string]struct{}{}
	for _, value := range allowed {
		if !outcomeValuePattern.MatchString(value) {
			return errors.New("outcome definition value is invalid")
		}
		if _, exists := seen[value]; exists {
			return errors.New("outcome definition values must be unique")
		}
		seen[value] = struct{}{}
	}
	if len(d.SuccessValues) == 0 {
		return errors.New("success_values is required")
	}
	successes := map[string]struct{}{}
	for _, value := range d.SuccessValues {
		if _, ok := seen[value]; !ok {
			return errors.New("success_values must be contained in allowed_values")
		}
		if _, exists := successes[value]; exists {
			return errors.New("success_values must be unique")
		}
		successes[value] = struct{}{}
	}
	return nil
}

func (d *OutcomeDefinition) GetRevision() uint64      { return d.Revision }
func (d *OutcomeDefinition) SetRevision(value uint64) { d.Revision = value }

func (d OutcomeDefinition) Allows(value string) bool {
	allowed := d.AllowedValues
	if d.DataType == OutcomeBoolean && len(allowed) == 0 {
		allowed = []string{"false", "true"}
	}
	return slices.Contains(allowed, value)
}

func (d OutcomeDefinition) Successful(value string) bool {
	return slices.Contains(d.SuccessValues, value)
}

type Outcome struct {
	ID                  string    `json:"id"`
	ProjectID           string    `json:"project_id"`
	WorkUnitID          string    `json:"work_unit_id"`
	DefinitionID        string    `json:"definition_id"`
	DefinitionVersion   uint64    `json:"definition_version"`
	Value               string    `json:"value"`
	ReporterKeyID       string    `json:"reporter_key_id"`
	EvidenceSHA256      string    `json:"evidence_sha256,omitempty"`
	EvidenceRef         string    `json:"evidence_ref,omitempty"`
	ObservedAt          time.Time `json:"observed_at"`
	IngestedAt          time.Time `json:"ingested_at"`
	SupersedesOutcomeID string    `json:"supersedes_outcome_id,omitempty"`
	Revision            uint64    `json:"revision"`
	GovernanceSequence  uint64    `json:"governance_sequence"`
	Provisional         bool      `json:"provisional"`
}

func ValidateEvidenceRef(value string) error {
	if len(value) > 128 || strings.ContainsAny(value, "\r\n") {
		return errors.New("evidence_ref is invalid")
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return errors.New("evidence_ref is invalid")
		}
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{"://", "bearer ", "api_key", "apikey", "token=", "secret=", "password=", "sk-", "gw_"} {
		if strings.Contains(lower, marker) {
			return errors.New("evidence_ref may not contain URLs or credentials")
		}
	}
	return nil
}

func (o Outcome) Validate() error {
	if !ValidOutcomeID(o.ID) || o.ProjectID == "" || !ValidWorkUnitID(o.WorkUnitID) ||
		!ValidOutcomeDefinitionID(o.DefinitionID) || o.DefinitionVersion == 0 ||
		o.ReporterKeyID == "" || !outcomeValuePattern.MatchString(o.Value) || o.ObservedAt.IsZero() || o.IngestedAt.IsZero() || o.Revision == 0 {
		return errors.New("outcome identity is invalid")
	}
	if o.EvidenceSHA256 != "" && !ValidEvidenceSHA256(o.EvidenceSHA256) {
		return errors.New("evidence_sha256 must be 64 lowercase hex characters")
	}
	return ValidateEvidenceRef(o.EvidenceRef)
}
