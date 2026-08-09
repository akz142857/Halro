package domain

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Model capability detection is a control-plane job. Its records deliberately
// contain only classifications and counters; provider payloads and model output
// are never part of this type and therefore cannot accidentally be persisted.
type ModelCapabilityDetectionStatus string

const (
	DetectionQueued      ModelCapabilityDetectionStatus = "queued"
	DetectionRunning     ModelCapabilityDetectionStatus = "running"
	DetectionCompleted   ModelCapabilityDetectionStatus = "completed"
	DetectionFailed      ModelCapabilityDetectionStatus = "failed"
	DetectionCanceled    ModelCapabilityDetectionStatus = "canceled"
	DetectionInterrupted ModelCapabilityDetectionStatus = "interrupted"
)

func (s ModelCapabilityDetectionStatus) Terminal() bool {
	switch s {
	case DetectionCompleted, DetectionFailed, DetectionCanceled, DetectionInterrupted:
		return true
	default:
		return false
	}
}

type CapabilityProbeStatus string

const (
	ProbeSupported    CapabilityProbeStatus = "supported"
	ProbeUnsupported  CapabilityProbeStatus = "unsupported"
	ProbeInconclusive CapabilityProbeStatus = "inconclusive"
	ProbeUnavailable  CapabilityProbeStatus = "unavailable"
	ProbeUnauthorized CapabilityProbeStatus = "unauthorized"
	ProbeNotProbed    CapabilityProbeStatus = "not_probed"
	ProbeCanceled     CapabilityProbeStatus = "canceled"
)

func (s CapabilityProbeStatus) Valid() bool {
	return slices.Contains([]CapabilityProbeStatus{ProbeSupported, ProbeUnsupported, ProbeInconclusive,
		ProbeUnavailable, ProbeUnauthorized, ProbeNotProbed, ProbeCanceled}, s)
}

type CapabilityProbeResult struct {
	Status      CapabilityProbeStatus `json:"status"`
	Evidence    CapabilityEvidence    `json:"evidence,omitempty"`
	ErrorClass  string                `json:"error_class,omitempty"`
	BindingID   string                `json:"binding_id"`
	ProbeKind   string                `json:"probe_kind"`
	StartedAt   *time.Time            `json:"started_at,omitempty"`
	CompletedAt *time.Time            `json:"completed_at,omitempty"`
}

// DetectionProviderCall is the durable accounting boundary around a possibly
// billable control-plane call. A running call recovered after a crash becomes
// unknown and is never replayed automatically.
type DetectionProviderCall struct {
	Sequence    int        `json:"sequence"`
	Capability  string     `json:"capability"`
	ProbeKind   string     `json:"probe_kind"`
	Status      string     `json:"status"` // reserved|running|completed|unknown
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	InputUnits  int64      `json:"input_units,omitempty"`
	OutputUnits int64      `json:"output_units,omitempty"`
}

type ModelCapabilityDetection struct {
	ID                   string                           `json:"id"`
	ProviderID           string                           `json:"provider_id"`
	ProviderRevision     uint64                           `json:"provider_revision"`
	CredentialRevision   uint64                           `json:"credential_revision"`
	CredentialKeyVersion uint16                           `json:"credential_key_version"`
	ProviderModel        string                           `json:"provider_model"`
	ModelRevision        string                           `json:"model_revision"`
	BindingID            string                           `json:"binding_id"`
	ProfileID            ProviderProfileID                `json:"profile_id"`
	AccessSurface        AccessSurface                    `json:"access_surface"`
	TargetKind           DeploymentTargetKind             `json:"target_kind"`
	CanonicalTarget      string                           `json:"canonical_target"`
	Region               string                           `json:"region,omitempty"`
	TargetFingerprint    string                           `json:"target_fingerprint"`
	DetectorVersion      string                           `json:"detector_version"`
	RiskTier             string                           `json:"risk_tier"`
	Status               ModelCapabilityDetectionStatus   `json:"status"`
	Source               string                           `json:"source"`
	Results              map[string]CapabilityProbeResult `json:"capabilities"`
	Recommended          ProviderCapabilities             `json:"recommended_capabilities"`
	ProviderCalls        int                              `json:"provider_calls"`
	MaxProviderCalls     int                              `json:"max_provider_calls"`
	Calls                []DetectionProviderCall          `json:"provider_call_records,omitempty"`
	StartedAt            *time.Time                       `json:"started_at,omitempty"`
	CompletedAt          *time.Time                       `json:"completed_at,omitempty"`
	ExpiresAt            *time.Time                       `json:"expires_at,omitempty"`
	CancelRequestedAt    *time.Time                       `json:"cancel_requested_at,omitempty"`
	ExpiryRecordedAt     *time.Time                       `json:"expiry_recorded_at,omitempty"`
	CreatedBy            string                           `json:"created_by"`
	IdempotencyKeyHash   string                           `json:"idempotency_key_hash"`
	RequestHash          string                           `json:"request_hash"`
	SelectionRevision    string                           `json:"selection_revision,omitempty"`
	ForceRefresh         bool                             `json:"-"`
	Revision             uint64                           `json:"revision"`
	CreatedAt            time.Time                        `json:"created_at"`
	UpdatedAt            time.Time                        `json:"updated_at"`
}

func (d *ModelCapabilityDetection) GetRevision() uint64  { return d.Revision }
func (d *ModelCapabilityDetection) SetRevision(v uint64) { d.Revision = v }
func (d ModelCapabilityDetection) Fresh(now time.Time) bool {
	return d.Status == DetectionCompleted && d.ExpiresAt != nil && now.Before(*d.ExpiresAt)
}

func (d ModelCapabilityDetection) Validate() error {
	var problems []error
	if d.ID == "" || d.ProviderID == "" || d.ProviderRevision == 0 || d.CredentialRevision == 0 ||
		d.ProviderModel == "" || d.ModelRevision == "" || d.BindingID == "" || d.ProfileID == "" ||
		d.AccessSurface == "" || d.TargetKind == "" || d.CanonicalTarget == "" || d.TargetFingerprint == "" ||
		d.DetectorVersion == "" || d.RiskTier != "safe_automatic" || d.CreatedBy == "" ||
		d.IdempotencyKeyHash == "" || d.RequestHash == "" {
		problems = append(problems, errors.New("capability detection identity is incomplete"))
	}
	if d.Status == "" || (d.Status != DetectionQueued && d.Status != DetectionRunning && !d.Status.Terminal()) {
		problems = append(problems, errors.New("capability detection status is invalid"))
	}
	if d.MaxProviderCalls < 0 || d.MaxProviderCalls > 8 || d.ProviderCalls < 0 || d.ProviderCalls > d.MaxProviderCalls || len(d.Calls) > d.MaxProviderCalls {
		problems = append(problems, errors.New("capability detection call budget is invalid"))
	}
	if d.ProviderCalls != len(d.Calls) {
		problems = append(problems, errors.New("capability detection call count does not match durable records"))
	}
	for index, call := range d.Calls {
		validStatus := slices.Contains([]string{"reserved", "running", "completed", "unknown"}, call.Status)
		if call.Sequence != index+1 || !slices.Contains(capabilityNames, call.Capability) || strings.TrimSpace(call.ProbeKind) == "" || !validStatus {
			problems = append(problems, fmt.Errorf("capability detection provider call %d is invalid", index+1))
		}
		if call.Status == "running" && call.StartedAt == nil || (call.Status == "completed" || call.Status == "unknown") && call.FinishedAt == nil {
			problems = append(problems, fmt.Errorf("capability detection provider call %d has invalid timestamps", index+1))
		}
	}
	if d.CreatedAt.IsZero() || d.UpdatedAt.IsZero() {
		problems = append(problems, errors.New("capability detection timestamps are required"))
	}
	for capability, result := range d.Results {
		if !slices.Contains(capabilityNames, capability) || !result.Status.Valid() || result.BindingID != d.BindingID || strings.TrimSpace(result.ProbeKind) == "" {
			problems = append(problems, fmt.Errorf("capability detection result %q is invalid", capability))
			continue
		}
		if result.Status == ProbeSupported && result.Evidence != EvidenceVerified {
			problems = append(problems, fmt.Errorf("supported capability %q requires verified evidence", capability))
		}
		if result.Status != ProbeSupported && result.Evidence == EvidenceVerified {
			problems = append(problems, fmt.Errorf("non-supported capability %q cannot carry verified evidence", capability))
		}
	}
	if d.Status == DetectionCompleted {
		if !d.Recommended.AnyOperation() || d.CompletedAt == nil ||
			(d.Source == "verified_probe" && (d.ExpiresAt == nil || !d.ExpiresAt.After(*d.CompletedAt))) {
			problems = append(problems, errors.New("completed capability detection requires a useful, expiring recommendation"))
		}
		if d.Source == "verified_probe" {
			if err := validateDetectionRecommendation(d); err != nil {
				problems = append(problems, err)
			}
		} else if d.Source != "builtin_catalog" {
			problems = append(problems, errors.New("completed capability detection source is invalid"))
		}
	}
	return errors.Join(problems...)
}

func validateDetectionRecommendation(d ModelCapabilityDetection) error {
	expected := ProviderCapabilities{MaxContextTokens: d.Recommended.MaxContextTokens, MaxOutputTokens: d.Recommended.MaxOutputTokens}
	for capability, result := range d.Results {
		if result.Status == ProbeSupported {
			setCapability(&expected, capability, true)
		}
	}
	if !ProviderCapabilitiesSubset(expected, d.Recommended) || !ProviderCapabilitiesSubset(d.Recommended, expected) {
		return errors.New("recommended capabilities must equal supported probe results")
	}
	if d.Recommended.Streaming && !d.Recommended.Chat || d.Recommended.StreamUsage && !d.Recommended.Streaming ||
		(d.Recommended.Tools || d.Recommended.Vision || d.Recommended.JSONMode || d.Recommended.DeveloperRole || d.Recommended.Reasoning) && !d.Recommended.Chat {
		return errors.New("recommended capability dependencies are incomplete")
	}
	return nil
}

func setCapability(c *ProviderCapabilities, name string, value bool) {
	switch name {
	case "chat":
		c.Chat = value
	case "streaming":
		c.Streaming = value
	case "embeddings":
		c.Embeddings = value
	case "moderations":
		c.Moderations = value
	case "images":
		c.Images = value
	case "transcriptions":
		c.Transcriptions = value
	case "speech":
		c.Speech = value
	case "files":
		c.Files = value
	case "batches":
		c.Batches = value
	case "rerank":
		c.Rerank = value
	case "async_generate":
		c.AsyncGenerate = value
	case "tools":
		c.Tools = value
	case "vision":
		c.Vision = value
	case "json_mode":
		c.JSONMode = value
	case "developer_role":
		c.DeveloperRole = value
	case "reasoning":
		c.Reasoning = value
	case "stream_usage":
		c.StreamUsage = value
	}
}

func DetectionCapabilitySnapshot(d ModelCapabilityDetection, retained ProviderCapabilities, at time.Time) ModelCapabilitySnapshot {
	snapshot := ModelCapabilitySnapshot{ProviderModel: d.ProviderModel, ModelRevision: d.ModelRevision,
		Source: "verified_probe", Status: "known", CapturedAt: at, Capabilities: retained}
	snapshot.Evidence = SnapshotEvidence(snapshot)
	return snapshot
}
