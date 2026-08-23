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

// MaxDetectionProviderCalls bounds one detection's total billable control-plane
// calls: the identification probes that establish which invocation interface
// answers, plus the capability plan on the one that did. Identification only
// spends extra calls on candidates that lose, and a probe against an interface
// the model does not serve fails before the provider does any work.
const MaxDetectionProviderCalls = 12

const (
	DetectionQueued      ModelCapabilityDetectionStatus = "queued"
	DetectionRunning     ModelCapabilityDetectionStatus = "running"
	DetectionCompleted   ModelCapabilityDetectionStatus = "completed"
	DetectionFailed      ModelCapabilityDetectionStatus = "failed"
	DetectionCanceled    ModelCapabilityDetectionStatus = "canceled"
	DetectionInterrupted ModelCapabilityDetectionStatus = "interrupted"
	// DetectionAmbiguous means identification answered on more than one
	// invocation interface. The deployment runs on exactly one, so the choice
	// is the operator's — but it is now made against probe evidence rather
	// than asked before anything is known.
	DetectionAmbiguous ModelCapabilityDetectionStatus = "ambiguous"
)

func (s ModelCapabilityDetectionStatus) Terminal() bool {
	switch s {
	case DetectionCompleted, DetectionFailed, DetectionCanceled, DetectionInterrupted, DetectionAmbiguous:
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
// unknown and is never replayed automatically. BindingID names the invocation
// interface the call went to, because identification spends calls on candidates
// the detection does not end up resolving to.
type DetectionProviderCall struct {
	Sequence    int        `json:"sequence"`
	BindingID   string     `json:"binding_id"`
	Capability  string     `json:"capability"`
	ProbeKind   string     `json:"probe_kind"`
	Status      string     `json:"status"` // reserved|running|completed|unknown
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	InputUnits  int64      `json:"input_units,omitempty"`
	OutputUnits int64      `json:"output_units,omitempty"`
}

// DetectionBindingCandidate is one invocation interface the model might run on,
// plus what identification learned about it. It carries a single decisive probe
// rather than a full result set: identification only has to establish whether
// the model answers on that Access Surface at all, and the winning candidate's
// probe is carried into Results so the capability pass never pays for it twice.
type DetectionBindingCandidate struct {
	BindingID     string            `json:"binding_id"`
	ProfileID     ProviderProfileID `json:"profile_id"`
	AccessSurface AccessSurface     `json:"access_surface"`
	ModelRevision string            `json:"model_revision"`
	// Verifiable names the capabilities this interface can establish by probing
	// at all. It is usually smaller than what the interface offers: generating
	// an image or transcribing audio costs real money and leaves artefacts, so
	// the safe-automatic plan has no probe for them. A model whose only
	// capabilities fall outside this set cannot be identified by asking — every
	// question the plan can put to it is one it cannot answer by construction —
	// and saying so is the difference between "detection failed" and "there was
	// nothing here detection could ask".
	Verifiable []string              `json:"verifiable,omitempty"`
	Capability string                `json:"capability,omitempty"`
	ProbeKind  string                `json:"probe_kind,omitempty"`
	Status     CapabilityProbeStatus `json:"status"`
	Evidence   CapabilityEvidence    `json:"evidence,omitempty"`
	ErrorClass string                `json:"error_class,omitempty"`
	Answered   bool                  `json:"answered"`
}

type ModelCapabilityDetection struct {
	ID                   string                      `json:"id"`
	ProviderID           string                      `json:"provider_id"`
	ProviderRevision     uint64                      `json:"provider_revision"`
	CredentialRevision   uint64                      `json:"credential_revision"`
	CredentialKeyVersion uint16                      `json:"credential_key_version"`
	ProviderModel        string                      `json:"provider_model"`
	ModelRevision        string                      `json:"model_revision"`
	Candidates           []DetectionBindingCandidate `json:"binding_candidates"`
	BindingID            string                      `json:"binding_id,omitempty"`
	ProfileID            ProviderProfileID           `json:"profile_id,omitempty"`
	AccessSurface        AccessSurface               `json:"access_surface,omitempty"`
	TargetKind           DeploymentTargetKind        `json:"target_kind"`
	CanonicalTarget      string                      `json:"canonical_target"`
	Region               string                      `json:"region,omitempty"`
	// SelectionFingerprint identifies what the operator asked to detect —
	// model, region, revisions and the candidate interfaces. It exists from
	// creation and is what refresh cooldown compares. TargetFingerprint
	// identifies the interface identification resolved to, so it can only
	// exist once one has been resolved, and it is what deployment creation
	// checks a reused detection against.
	SelectionFingerprint string                           `json:"selection_fingerprint"`
	TargetFingerprint    string                           `json:"target_fingerprint,omitempty"`
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
		d.ProviderModel == "" || d.ModelRevision == "" || len(d.Candidates) == 0 ||
		d.TargetKind == "" || d.CanonicalTarget == "" || d.SelectionFingerprint == "" ||
		d.DetectorVersion == "" || d.RiskTier != "safe_automatic" || d.CreatedBy == "" ||
		d.IdempotencyKeyHash == "" || d.RequestHash == "" {
		problems = append(problems, errors.New("capability detection identity is incomplete"))
	}
	seenCandidates := make(map[string]struct{}, len(d.Candidates))
	for _, candidate := range d.Candidates {
		if candidate.BindingID == "" || candidate.ProfileID == "" || candidate.AccessSurface == "" || !candidate.Status.Valid() {
			problems = append(problems, fmt.Errorf("capability detection candidate %q is invalid", candidate.BindingID))
			continue
		}
		if _, duplicate := seenCandidates[candidate.BindingID]; duplicate {
			problems = append(problems, fmt.Errorf("capability detection candidate %q is listed twice", candidate.BindingID))
		}
		seenCandidates[candidate.BindingID] = struct{}{}
		if candidate.Capability != "" && !slices.Contains(capabilityNames, candidate.Capability) {
			problems = append(problems, fmt.Errorf("capability detection candidate %q names an unknown capability", candidate.BindingID))
		}
		for _, name := range candidate.Verifiable {
			if !slices.Contains(capabilityNames, name) {
				problems = append(problems, fmt.Errorf("capability detection candidate %q claims an unknown verifiable capability %q", candidate.BindingID, name))
			}
		}
		// A probe outcome can only come from a capability the interface was able
		// to ask about in the first place.
		if candidate.Capability != "" && len(candidate.Verifiable) > 0 && !slices.Contains(candidate.Verifiable, candidate.Capability) {
			problems = append(problems, fmt.Errorf("capability detection candidate %q reports a probe it could not run", candidate.BindingID))
		}
		// Answered is the resolution input, so it may never be more generous
		// than the probe that produced it.
		if candidate.Answered != (candidate.Status == ProbeSupported) {
			problems = append(problems, fmt.Errorf("capability detection candidate %q claims an outcome its probe does not support", candidate.BindingID))
		}
	}
	if d.BindingID != "" {
		if _, known := seenCandidates[d.BindingID]; !known {
			problems = append(problems, errors.New("capability detection resolved a binding that was never a candidate"))
		}
		if d.ProfileID == "" || d.AccessSurface == "" || d.TargetFingerprint == "" {
			problems = append(problems, errors.New("resolved capability detection binding is incomplete"))
		}
	} else if d.ProfileID != "" || d.AccessSurface != "" || d.TargetFingerprint != "" {
		problems = append(problems, errors.New("capability detection names a target without resolving a binding"))
	}
	if d.Status == "" || (d.Status != DetectionQueued && d.Status != DetectionRunning && !d.Status.Terminal()) {
		problems = append(problems, errors.New("capability detection status is invalid"))
	}
	if d.MaxProviderCalls < 0 || d.MaxProviderCalls > MaxDetectionProviderCalls || d.ProviderCalls < 0 || d.ProviderCalls > d.MaxProviderCalls || len(d.Calls) > d.MaxProviderCalls {
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
		if _, known := seenCandidates[call.BindingID]; !known {
			problems = append(problems, fmt.Errorf("capability detection provider call %d was spent on a binding that was never a candidate", index+1))
		}
		if call.Status == "running" && call.StartedAt == nil || (call.Status == "completed" || call.Status == "unknown") && call.FinishedAt == nil {
			problems = append(problems, fmt.Errorf("capability detection provider call %d has invalid timestamps", index+1))
		}
	}
	if d.CreatedAt.IsZero() || d.UpdatedAt.IsZero() {
		problems = append(problems, errors.New("capability detection timestamps are required"))
	}
	if len(d.Results) > 0 && d.BindingID == "" {
		problems = append(problems, errors.New("capability detection carries results without a resolved binding"))
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
	if d.Status == DetectionAmbiguous {
		answered := 0
		for _, candidate := range d.Candidates {
			if candidate.Answered {
				answered++
			}
		}
		if answered < 2 || d.CompletedAt == nil || d.BindingID != "" {
			problems = append(problems, errors.New("ambiguous capability detection requires two answering candidates and no resolved binding"))
		}
	}
	if d.Status == DetectionCompleted {
		if d.BindingID == "" {
			problems = append(problems, errors.New("completed capability detection must resolve one binding"))
		}
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
	case "fetched_image":
		c.FetchedImage = value
	case "json_mode":
		c.JSONMode = value
	case "developer_role":
		c.DeveloperRole = value
	case "reasoning":
		c.Reasoning = value
	case "stream_usage":
		c.StreamUsage = value
	case "provider_executed_tools":
		c.ProviderExecutedTools = value
	}
}

func DetectionCapabilitySnapshot(d ModelCapabilityDetection, retained ProviderCapabilities, at time.Time) ModelCapabilitySnapshot {
	snapshot := ModelCapabilitySnapshot{ProviderModel: d.ProviderModel, ModelRevision: d.ModelRevision,
		Source: "verified_probe", Status: "known", CapturedAt: at, Capabilities: retained}
	snapshot.Evidence = SnapshotEvidence(snapshot)
	return snapshot
}
