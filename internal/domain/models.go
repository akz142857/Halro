package domain

import (
	"errors"
	"net/netip"
	"strings"
	"time"
)

type ProviderType string

const (
	ProviderOpenAI           ProviderType = "openai"
	ProviderAnthropic        ProviderType = "anthropic"
	ProviderAzureOpenAI      ProviderType = "azure_openai"
	ProviderDeepSeek         ProviderType = "deepseek"
	ProviderOpenAICompatible ProviderType = "openai_compatible"
	ProviderBedrock          ProviderType = "bedrock"
	ProviderGemini           ProviderType = "gemini"
)

type ProviderResourceKind string

const (
	ResourceFile        ProviderResourceKind = "file"
	ResourceBatch       ProviderResourceKind = "batch"
	ResourceAsyncInvoke ProviderResourceKind = "async_invoke"
)

type ProviderResource struct {
	ID                 string               `json:"id"`
	Kind               ProviderResourceKind `json:"kind"`
	ProjectID          string               `json:"project_id"`
	ProviderID         string               `json:"provider_id"`
	DeploymentID       string               `json:"deployment_id"`
	PublicModel        string               `json:"public_model"`
	ProfileID          ProviderProfileID    `json:"profile_id"`
	Region             string               `json:"region"`
	UpstreamID         string               `json:"upstream_id"`
	IdempotencyKeyHash [32]byte             `json:"idempotency_key_hash"`
	RequestFingerprint [32]byte             `json:"request_fingerprint"`
	CreationStatus     string               `json:"creation_status"`
	// ReservedBy names the process that wrote the reservation. A reservation
	// still held by a process that is gone can never complete on its own, and
	// the data directory is exclusive, so a value other than the running
	// instance's means the request that made it died.
	ReservedBy        string    `json:"reserved_by,omitempty"`
	CleanupStatus     string    `json:"cleanup_status,omitempty"`
	Status            string    `json:"status"`
	ObjectPath        string    `json:"object_path,omitempty"`
	ObjectContentType string    `json:"object_content_type,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	ExpiresAt         time.Time `json:"expires_at"`
	Revision          uint64    `json:"revision"`
}

func (r *ProviderResource) GetRevision() uint64  { return r.Revision }
func (r *ProviderResource) SetRevision(v uint64) { r.Revision = v }
func (r ProviderResource) Validate() error {
	var problems []error
	if r.ID == "" || r.ProjectID == "" || r.ProviderID == "" || r.DeploymentID == "" || r.ProfileID == "" || r.PublicModel == "" {
		problems = append(problems, errors.New("resource identity and owner are required"))
	}
	if r.Kind != ResourceFile && r.Kind != ResourceBatch && r.Kind != ResourceAsyncInvoke {
		problems = append(problems, errors.New("resource kind is invalid"))
	}
	if r.CreationStatus == "" || r.Status == "" || r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() || r.ExpiresAt.IsZero() || !r.ExpiresAt.After(r.CreatedAt) {
		problems = append(problems, errors.New("resource lifecycle is invalid"))
	}
	if r.CleanupStatus != "" && r.CleanupStatus != "deleting" && r.CleanupStatus != "pending" {
		problems = append(problems, errors.New("resource cleanup status is invalid"))
	}
	return errors.Join(problems...)
}

// ExpiryReapable prevents TTL maintenance from discarding the only owner
// mapping for an operation that may still be running upstream.
func (r ProviderResource) ExpiryReapable() bool {
	if r.CreationStatus != "completed" {
		return false
	}
	if r.Kind == ResourceFile {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(r.Status)) {
	case "completed", "failed", "expired", "cancelled", "canceled", "succeeded":
		return true
	default:
		return false
	}
}

type Credential struct {
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	Type          ProviderType     `json:"type"`
	AccessSurface AccessSurface    `json:"access_surface"`
	Scheme        CredentialScheme `json:"scheme"`
	Audience      string           `json:"audience"`
	Ciphertext    []byte           `json:"ciphertext"`
	KeyVersion    uint16           `json:"key_version"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
	Revision      uint64           `json:"revision"`
}

func (c *Credential) GetRevision() uint64      { return c.Revision }
func (c *Credential) SetRevision(value uint64) { c.Revision = value }

func (c Credential) Validate() error {
	var problems []error
	if c.ID == "" {
		problems = append(problems, errors.New("credential id is required"))
	}
	if strings.TrimSpace(c.Name) == "" {
		problems = append(problems, errors.New("credential name is required"))
	}
	if c.Type == "" {
		problems = append(problems, errors.New("credential type is required"))
	}
	_, knownType := DefaultProviderProfile(c.Type)
	resolved, compatible := ResolveCredentialProfile(c.Type, c.AccessSurface, c.Scheme)
	compatible = compatible && c.AccessSurface == resolved.AccessSurface && c.Scheme == resolved.CredentialScheme
	if knownType && !compatible {
		problems = append(problems, errors.New("credential access surface or scheme is incompatible"))
	} else if !knownType && (c.AccessSurface != "" || c.Scheme != "") {
		problems = append(problems, errors.New("custom credential types cannot declare a provider access surface or scheme"))
	}
	if strings.TrimSpace(c.Audience) == "" {
		problems = append(problems, errors.New("credential audience is required"))
	}
	if len(c.Ciphertext) == 0 {
		problems = append(problems, errors.New("credential ciphertext is required"))
	}
	return errors.Join(problems...)
}

type Project struct {
	ID                   string         `json:"id"`
	Name                 string         `json:"name"`
	Enabled              bool           `json:"enabled"`
	AllowedRoutes        []string       `json:"allowed_routes"`
	RPM                  int64          `json:"rpm"`
	TPM                  int64          `json:"tpm"`
	MaxConcurrency       int64          `json:"max_concurrency"`
	DailyBudgetMicrosUSD int64          `json:"daily_budget_micros_usd"`
	MaxInputTokens       int64          `json:"max_input_tokens"`
	MaxOutputTokens      int64          `json:"max_output_tokens"`
	MaxRequestBytes      int64          `json:"max_request_bytes"`
	MaxStreamDuration    time.Duration  `json:"max_stream_duration"`
	AllowedCIDRs         []netip.Prefix `json:"allowed_cidrs"`
	RedactionPolicyID    string         `json:"redaction_policy_id"`
	TokenGuardPolicyID   string         `json:"token_guard_policy_id"`
	CreatedAt            time.Time      `json:"created_at"`
	UpdatedAt            time.Time      `json:"updated_at"`
	Revision             uint64         `json:"revision"`
	DeletedAt            *time.Time     `json:"deleted_at,omitempty"`
}

func (p *Project) GetRevision() uint64      { return p.Revision }
func (p *Project) SetRevision(value uint64) { p.Revision = value }

// MaxProjectNameLength bounds the stored name so every caller — console, script, or
// future front end — hits the same limit. The web form mirrors this value.
const MaxProjectNameLength = 128

func (p Project) Validate() error {
	var problems []error
	if p.ID == "" {
		problems = append(problems, errors.New("project id is required"))
	}
	if name := strings.TrimSpace(p.Name); name == "" {
		problems = append(problems, errors.New("project name is required"))
	} else if len([]rune(name)) > MaxProjectNameLength {
		problems = append(problems, errors.New("project name is too long"))
	}
	for name, value := range map[string]int64{
		"rpm":                     p.RPM,
		"tpm":                     p.TPM,
		"max_concurrency":         p.MaxConcurrency,
		"daily_budget_micros_usd": p.DailyBudgetMicrosUSD,
		"max_input_tokens":        p.MaxInputTokens,
		"max_output_tokens":       p.MaxOutputTokens,
		"max_request_bytes":       p.MaxRequestBytes,
	} {
		if value < 0 {
			problems = append(problems, errors.New(name+" cannot be negative"))
		}
	}
	return errors.Join(problems...)
}

// GatewayKey is a persistence model, not a response model: the metadata store encodes it
// with encoding/json, so KeyHash cannot be tagged json:"-" without dropping the hash on
// write. Admin handlers must serialise gatewayKeyView instead of this struct — the
// TestAdminKeyResponsesNeverExposeKeyHash regression test enforces that.
type GatewayKey struct {
	ID          string     `json:"id"`
	ProjectID   string     `json:"project_id"`
	Name        string     `json:"name"`
	HashVersion uint16     `json:"hash_version"`
	KeyHash     [32]byte   `json:"key_hash"`
	Enabled     bool       `json:"enabled"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	Revision    uint64     `json:"revision"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
}

type ProviderInstance struct {
	ID                     string                   `json:"id"`
	Name                   string                   `json:"name"`
	Type                   ProviderType             `json:"type"`
	BaseURL                string                   `json:"base_url"`
	APIVersion             string                   `json:"api_version,omitempty"`
	CredentialID           string                   `json:"credential_id"`
	AccessSurface          AccessSurface            `json:"access_surface"`
	ProfileID              ProviderProfileID        `json:"profile_id"`
	CredentialScheme       CredentialScheme         `json:"credential_scheme"`
	AllowedHosts           []string                 `json:"allowed_hosts"`
	Capabilities           ProviderCapabilities     `json:"capabilities"`
	CapabilityEvidence     CapabilityEvidenceSet    `json:"capability_evidence"`
	Bindings               []ProviderProfileBinding `json:"bindings,omitempty"`
	MaxConcurrency         int64                    `json:"max_concurrency"`
	Enabled                bool                     `json:"enabled"`
	LastTestStatus         DeploymentTestStatus     `json:"last_test_status,omitempty"`
	LastTestedAt           *time.Time               `json:"last_tested_at,omitempty"`
	LastTestLatencyMillis  int64                    `json:"last_test_latency_millis,omitempty"`
	LastTestErrorClass     string                   `json:"last_test_error_class,omitempty"`
	LastTestRevision       uint64                   `json:"last_test_revision,omitempty"`
	LastTestHealthyTargets int                      `json:"last_test_healthy_targets,omitempty"`
	LastTestTotalTargets   int                      `json:"last_test_total_targets,omitempty"`
	CreatedAt              time.Time                `json:"created_at"`
	UpdatedAt              time.Time                `json:"updated_at"`
	Revision               uint64                   `json:"revision"`
	DeletedAt              *time.Time               `json:"deleted_at,omitempty"`
}

// ProviderProfileBinding binds one immutable provider protocol profile to a
// connection. The provider's legacy profile fields remain a projection of the
// first binding while v1 clients are supported.
type ProviderProfileBinding struct {
	ID                 string                `json:"id"`
	ProviderID         string                `json:"provider_id"`
	ProfileID          ProviderProfileID     `json:"profile_id"`
	AccessSurface      AccessSurface         `json:"access_surface"`
	CredentialScheme   CredentialScheme      `json:"credential_scheme"`
	Capabilities       ProviderCapabilities  `json:"capabilities"`
	CapabilityEvidence CapabilityEvidenceSet `json:"capability_evidence"`
	Enabled            bool                  `json:"enabled"`
}

func DefaultProviderProfileBindingID(providerID string, profileID ProviderProfileID) string {
	return providerID + ":" + string(profileID)
}

func (b ProviderProfileBinding) Validate(providerID string, providerType ProviderType) error {
	if strings.TrimSpace(b.ID) == "" {
		return errors.New("provider profile binding id is required")
	}
	if b.ProviderID != providerID {
		return errors.New("provider profile binding provider id is incompatible")
	}
	if err := ValidateProviderProfile(providerType, b.AccessSurface, b.ProfileID, b.CredentialScheme); err != nil {
		return err
	}
	if b.Enabled && !b.Capabilities.AnyOperation() {
		return errors.New("provider profile binding must declare at least one operation capability")
	}
	if b.Capabilities.Streaming && !b.Capabilities.Chat {
		return errors.New("streaming capability requires chat capability")
	}
	if err := b.CapabilityEvidence.Validate(b.Capabilities); err != nil {
		return err
	}
	return nil
}

func (p ProviderInstance) EffectiveProfileBindings() []ProviderProfileBinding {
	if len(p.Bindings) != 0 {
		result := make([]ProviderProfileBinding, len(p.Bindings))
		copy(result, p.Bindings)
		return result
	}
	return []ProviderProfileBinding{{
		ID: DefaultProviderProfileBindingID(p.ID, p.ProfileID), ProviderID: p.ID, ProfileID: p.ProfileID,
		AccessSurface: p.AccessSurface, CredentialScheme: p.CredentialScheme,
		Capabilities: p.Capabilities, CapabilityEvidence: p.CapabilityEvidence.Clone(), Enabled: p.Enabled,
	}}
}

func (p ProviderInstance) ProfileBinding(id string) (ProviderProfileBinding, bool) {
	for _, binding := range p.EffectiveProfileBindings() {
		if binding.ID == id {
			return binding, true
		}
	}
	return ProviderProfileBinding{}, false
}

func BindingsCapabilitiesSummary(bindings []ProviderProfileBinding) (ProviderCapabilities, CapabilityEvidenceSet) {
	var summary ProviderCapabilities
	evidence := EvidenceForCapabilities(summary, EvidenceDeclared)
	for _, binding := range bindings {
		if !binding.Enabled {
			continue
		}
		c := binding.Capabilities
		summary.Chat = summary.Chat || c.Chat
		summary.Streaming = summary.Streaming || c.Streaming
		summary.Embeddings = summary.Embeddings || c.Embeddings
		summary.Moderations = summary.Moderations || c.Moderations
		summary.Images = summary.Images || c.Images
		summary.Transcriptions = summary.Transcriptions || c.Transcriptions
		summary.Speech = summary.Speech || c.Speech
		summary.Files = summary.Files || c.Files
		summary.Batches = summary.Batches || c.Batches
		summary.Rerank = summary.Rerank || c.Rerank
		summary.AsyncGenerate = summary.AsyncGenerate || c.AsyncGenerate
		summary.Tools = summary.Tools || c.Tools
		summary.Vision = summary.Vision || c.Vision
		summary.JSONMode = summary.JSONMode || c.JSONMode
		summary.DeveloperRole = summary.DeveloperRole || c.DeveloperRole
		summary.Reasoning = summary.Reasoning || c.Reasoning
		summary.StreamUsage = summary.StreamUsage || c.StreamUsage
		if c.MaxContextTokens > summary.MaxContextTokens {
			summary.MaxContextTokens = c.MaxContextTokens
		}
		if c.MaxOutputTokens > summary.MaxOutputTokens {
			summary.MaxOutputTokens = c.MaxOutputTokens
		}
		for name, value := range binding.CapabilityEvidence {
			if evidenceRank(value) > evidenceRank(evidence[name]) {
				evidence[name] = value
			}
		}
	}
	evidence = NormalizeCapabilityEvidence(summary, evidence, EvidenceDeclared)
	return summary, evidence
}

type ProviderCapabilities struct {
	Chat             bool  `json:"chat"`
	Streaming        bool  `json:"streaming"`
	Embeddings       bool  `json:"embeddings"`
	Moderations      bool  `json:"moderations"`
	Images           bool  `json:"images"`
	Transcriptions   bool  `json:"transcriptions"`
	Speech           bool  `json:"speech"`
	Files            bool  `json:"files"`
	Batches          bool  `json:"batches"`
	Rerank           bool  `json:"rerank"`
	AsyncGenerate    bool  `json:"async_generate"`
	Tools            bool  `json:"tools"`
	Vision           bool  `json:"vision"`
	JSONMode         bool  `json:"json_mode"`
	DeveloperRole    bool  `json:"developer_role"`
	Reasoning        bool  `json:"reasoning"`
	StreamUsage      bool  `json:"stream_usage"`
	MaxContextTokens int64 `json:"max_context_tokens"`
	MaxOutputTokens  int64 `json:"max_output_tokens"`
}

func (p *ProviderInstance) GetRevision() uint64      { return p.Revision }
func (p *ProviderInstance) SetRevision(value uint64) { p.Revision = value }

func (p ProviderInstance) Validate() error {
	var problems []error
	if p.ID == "" {
		problems = append(problems, errors.New("provider id is required"))
	}
	if strings.TrimSpace(p.Name) == "" {
		problems = append(problems, errors.New("provider name is required"))
	}
	switch p.Type {
	case ProviderOpenAI, ProviderAnthropic, ProviderAzureOpenAI, ProviderDeepSeek, ProviderOpenAICompatible, ProviderGemini, ProviderBedrock:
	default:
		problems = append(problems, errors.New("provider type is not implemented"))
	}
	if p.Type == ProviderAzureOpenAI && strings.TrimSpace(p.APIVersion) == "" {
		problems = append(problems, errors.New("azure openai api version is required"))
	}
	if err := ValidateProviderProfile(p.Type, p.AccessSurface, p.ProfileID, p.CredentialScheme); err != nil {
		problems = append(problems, err)
	}
	if strings.TrimSpace(p.BaseURL) == "" {
		problems = append(problems, errors.New("provider base URL is required"))
	}
	if p.CredentialID == "" {
		problems = append(problems, errors.New("provider credential id is required"))
	}
	if len(p.AllowedHosts) == 0 {
		problems = append(problems, errors.New("provider allowed hosts must not be empty"))
	}
	capabilities := p.Capabilities
	if !capabilities.AnyOperation() && (p.Enabled || len(p.Bindings) == 0) {
		problems = append(problems, errors.New("provider must declare at least one operation capability"))
	}
	if capabilities.Streaming && !capabilities.Chat {
		problems = append(problems, errors.New("streaming capability requires chat capability"))
	}
	if capabilities.MaxContextTokens < 0 || capabilities.MaxOutputTokens < 0 ||
		(capabilities.MaxContextTokens > 0 && capabilities.MaxOutputTokens > capabilities.MaxContextTokens) {
		problems = append(problems, errors.New("provider capability token limits are invalid"))
	}
	if err := p.CapabilityEvidence.Validate(capabilities); err != nil {
		problems = append(problems, err)
	}
	seenBindings := make(map[string]struct{}, len(p.Bindings))
	seenProfiles := make(map[ProviderProfileID]struct{}, len(p.Bindings))
	enabledBinding := false
	for _, binding := range p.Bindings {
		if _, duplicate := seenBindings[binding.ID]; duplicate {
			problems = append(problems, errors.New("provider profile binding ids must be unique"))
			continue
		}
		seenBindings[binding.ID] = struct{}{}
		if _, duplicate := seenProfiles[binding.ProfileID]; duplicate {
			problems = append(problems, errors.New("provider profile bindings must not repeat a profile"))
		}
		seenProfiles[binding.ProfileID] = struct{}{}
		enabledBinding = enabledBinding || binding.Enabled
		if err := binding.Validate(p.ID, p.Type); err != nil {
			problems = append(problems, err)
		}
		if binding.CredentialScheme != p.CredentialScheme {
			problems = append(problems, errors.New("provider profile binding credential scheme is incompatible with connection"))
		}
		if binding.AccessSurface != p.AccessSurface {
			problems = append(problems, errors.New("provider profile binding access surface is incompatible with connection"))
		}
	}
	if p.Enabled && len(p.Bindings) != 0 && !enabledBinding {
		problems = append(problems, errors.New("enabled provider requires an enabled profile binding"))
	}
	if len(p.Bindings) != 0 {
		primary := p.Bindings[0]
		if primary.ProfileID != p.ProfileID || primary.AccessSurface != p.AccessSurface || primary.CredentialScheme != p.CredentialScheme {
			problems = append(problems, errors.New("provider legacy profile projection must match the primary binding"))
		}
		summary, evidence := BindingsCapabilitiesSummary(p.Bindings)
		if !ProviderCapabilitiesSubset(p.Capabilities, summary) || !ProviderCapabilitiesSubset(summary, p.Capabilities) {
			problems = append(problems, errors.New("provider capabilities must equal the profile binding summary"))
		}
		for name, value := range evidence {
			if p.CapabilityEvidence[name] != value {
				problems = append(problems, errors.New("provider capability evidence must equal the profile binding summary"))
				break
			}
		}
	}
	if p.MaxConcurrency < 0 {
		problems = append(problems, errors.New("provider max concurrency cannot be negative"))
	}
	if p.LastTestLatencyMillis < 0 || p.LastTestHealthyTargets < 0 || p.LastTestTotalTargets < 0 || p.LastTestHealthyTargets > p.LastTestTotalTargets {
		problems = append(problems, errors.New("provider test result is invalid"))
	}
	if p.LastTestStatus != "" {
		if p.LastTestStatus != DeploymentTestHealthy && p.LastTestStatus != DeploymentTestUnhealthy {
			problems = append(problems, errors.New("provider test status is invalid"))
		}
		if p.LastTestedAt == nil || p.LastTestRevision == 0 || p.LastTestTotalTargets == 0 {
			problems = append(problems, errors.New("provider test status requires timestamp, revision, and target count"))
		}
	}
	return errors.Join(problems...)
}

func DefaultProviderCapabilities(providerType ProviderType) ProviderCapabilities {
	switch providerType {
	case ProviderOpenAI, ProviderAzureOpenAI:
		return ProviderCapabilities{
			Chat: true, Streaming: true, Embeddings: true, Tools: true,
			Vision: true, JSONMode: true, DeveloperRole: true, Reasoning: true,
			StreamUsage: true,
		}
	case ProviderAnthropic:
		return ProviderCapabilities{Chat: true, Streaming: true, Tools: true, Vision: true, Reasoning: true, StreamUsage: true}
	case ProviderDeepSeek:
		return ProviderCapabilities{
			Chat: true, Streaming: true, Tools: true, JSONMode: true,
			Reasoning: true, StreamUsage: true,
		}
	case ProviderOpenAICompatible:
		// Compatibility servers vary. These conservative defaults preserve the
		// two core OpenAI endpoints; optional features must be explicitly enabled.
		return ProviderCapabilities{Chat: true, Streaming: true, Embeddings: true}
	case ProviderGemini:
		// Beta profile intentionally declares only the translated text subset.
		return ProviderCapabilities{Chat: true, Streaming: true, Embeddings: true, DeveloperRole: true}
	case ProviderBedrock:
		// Beta profile intentionally declares only Converse text chat and usage.
		return ProviderCapabilities{Chat: true, Streaming: true, StreamUsage: true}
	default:
		return ProviderCapabilities{}
	}
}

func DefaultProviderCapabilitiesForProfile(providerType ProviderType, profileID ProviderProfileID) ProviderCapabilities {
	switch profileID {
	case ProfileOpenAIMediaResources:
		return ProviderCapabilities{Moderations: true, Images: true, Transcriptions: true, Speech: true, Files: true, Batches: true}
	case ProfileBedrockInvokeTitanEmbedV2:
		return ProviderCapabilities{Embeddings: true, MaxContextTokens: 8192}
	case ProfileBedrockInvokeTitanImageV2:
		return ProviderCapabilities{Images: true}
	case ProfileBedrockAgentRerankCohere35:
		return ProviderCapabilities{Rerank: true}
	case ProfileBedrockAsyncNovaReel:
		return ProviderCapabilities{AsyncGenerate: true}
	case ProfileBedrockMantleOpenAIChat:
		return ProviderCapabilities{Chat: true, Streaming: true, Tools: true, Vision: true, JSONMode: true, DeveloperRole: true, Reasoning: true, StreamUsage: true}
	case ProfileBedrockMantleOpenAIResponses:
		// Phase 1C deliberately exposes only the stateless Responses subset. The
		// current canonical response mapper cannot preserve reasoning items.
		return ProviderCapabilities{Chat: true, Streaming: true, Tools: true, Vision: true, JSONMode: true, DeveloperRole: true, StreamUsage: true}
	case ProfileBedrockMantleAnthropicMessages:
		return ProviderCapabilities{Chat: true, Streaming: true, Tools: true, Vision: true, Reasoning: true, StreamUsage: true}
	default:
		return DefaultProviderCapabilities(providerType)
	}
}

// Deployment is a concrete model endpoint hosted by a Provider instance.
// Provider owns transport and credentials; Deployment owns model capabilities,
// price and target-level capacity. Routes reference Deployments and never
// duplicate Provider secrets or endpoint configuration.
type DeploymentTargetKind string

const (
	TargetModelID                      DeploymentTargetKind = "model_id"
	TargetAzureDeployment              DeploymentTargetKind = "azure_deployment"
	TargetBedrockFoundationModel       DeploymentTargetKind = "bedrock_foundation_model"
	TargetBedrockInferenceProfile      DeploymentTargetKind = "bedrock_inference_profile"
	TargetBedrockProvisionedThroughput DeploymentTargetKind = "bedrock_provisioned_throughput"
	TargetCustomEndpointModel          DeploymentTargetKind = "custom_endpoint_model"
)

// CapabilityReviewState tracks whether a deployment's stored capability
// snapshot still matches what the catalog and the running profile say.
type CapabilityReviewState string

const (
	// CapabilityReviewCurrent means the snapshot still matches.
	CapabilityReviewCurrent CapabilityReviewState = "current"
	// CapabilityReviewAvailable means the catalog now establishes more than the
	// snapshot holds. Nothing is enabled by it; an operator reviews and retests.
	CapabilityReviewAvailable CapabilityReviewState = "review_available"
	// CapabilityReviewDrifted means the snapshot claims more than the catalog or
	// the running profile now supports. Drifted deployments are fail-closed.
	CapabilityReviewDrifted CapabilityReviewState = "drifted"
)

func (s CapabilityReviewState) Valid() bool {
	switch s {
	case CapabilityReviewCurrent, CapabilityReviewAvailable, CapabilityReviewDrifted:
		return true
	default:
		return false
	}
}

// ModelCapabilitySnapshot is what was established about a model at the moment a
// deployment was saved. The request path reads this, never the live catalog: a
// catalog refresh must not be able to change what an enabled deployment does.
type ModelCapabilitySnapshot struct {
	ProviderModel string `json:"provider_model"`
	// ModelRevision is the per-model catalog digest this snapshot was taken
	// from. Drift is detected by comparing it, not the catalog-wide digest.
	ModelRevision   string               `json:"model_revision"`
	CatalogRevision string               `json:"catalog_revision,omitempty"`
	Source          string               `json:"source"`
	Status          string               `json:"status"`
	CapturedAt      time.Time            `json:"captured_at"`
	Capabilities    ProviderCapabilities `json:"capabilities"`
}

// DeclaredCapabilitySnapshot records an operator's own claim about a model.
// The revision still comes from the catalog — for a model it does not cover
// that is the digest of the "nothing established" entry, which is what later
// tells us the catalog has since started covering it.
func DeclaredCapabilitySnapshot(model, revision string, capabilities ProviderCapabilities, at time.Time) ModelCapabilitySnapshot {
	return ModelCapabilitySnapshot{
		ProviderModel: model, ModelRevision: revision,
		Source: "operator_declared", Status: "partial",
		CapturedAt: at, Capabilities: capabilities,
	}
}

func (s ModelCapabilitySnapshot) Validate(deployment Deployment) error {
	var problems []error
	if strings.TrimSpace(s.ProviderModel) == "" {
		problems = append(problems, errors.New("capability snapshot requires a provider model"))
	}
	if s.ProviderModel != deployment.ProviderModel {
		problems = append(problems, errors.New("capability snapshot model does not match the deployment"))
	}
	if strings.TrimSpace(s.Source) == "" || strings.TrimSpace(s.Status) == "" {
		problems = append(problems, errors.New("capability snapshot requires a source and status"))
	}
	if s.ModelRevision == "" {
		problems = append(problems, errors.New("capability snapshot requires a model revision"))
	}
	if s.CapturedAt.IsZero() {
		problems = append(problems, errors.New("capability snapshot requires a capture time"))
	}
	// The deployment may narrow what was established, never exceed it.
	if !ProviderCapabilitiesSubset(deployment.Capabilities, s.Capabilities) {
		problems = append(problems, errors.New("deployment capabilities exceed the capability snapshot"))
	}
	return errors.Join(problems...)
}

type Deployment struct {
	ID                      string                `json:"id"`
	Name                    string                `json:"name"`
	ProviderID              string                `json:"provider_id"`
	ProviderModel           string                `json:"provider_model"`
	TargetKind              DeploymentTargetKind  `json:"target_kind,omitempty"`
	AccessSurface           AccessSurface         `json:"access_surface"`
	ProfileID               ProviderProfileID     `json:"profile_id"`
	BindingID               string                `json:"binding_id,omitempty"`
	Region                  string                `json:"region"`
	Capabilities            ProviderCapabilities  `json:"capabilities"`
	CapabilityEvidence      CapabilityEvidenceSet `json:"capability_evidence"`
	InputMicrosPerMillion   int64                 `json:"input_micros_per_million"`
	OutputMicrosPerMillion  int64                 `json:"output_micros_per_million"`
	FixedRequestMicrosUSD   int64                 `json:"fixed_request_micros_usd"`
	MaxConcurrency          int64                 `json:"max_concurrency"`
	Priority                int                   `json:"priority"`
	Weight                  int                   `json:"weight"`
	Enabled                 bool                  `json:"enabled"`
	LastTestStatus          DeploymentTestStatus  `json:"last_test_status,omitempty"`
	LastTestedAt            *time.Time            `json:"last_tested_at,omitempty"`
	LastTestLatencyMillis   int64                 `json:"last_test_latency_millis,omitempty"`
	LastTestErrorClass      string                `json:"last_test_error_class,omitempty"`
	LastTestRevision        uint64                `json:"last_test_revision,omitempty"`
	CreatedAt               time.Time             `json:"created_at"`
	UpdatedAt               time.Time             `json:"updated_at"`
	Revision                uint64                `json:"revision"`
	DeletedAt               *time.Time            `json:"deleted_at,omitempty"`
	PricingQuarantined      bool                  `json:"pricing_quarantined,omitempty"`
	PricingQuarantineReason string                `json:"pricing_quarantine_reason,omitempty"`

	ModelCapabilitySnapshot ModelCapabilitySnapshot `json:"model_capability_snapshot"`
	CapabilityReviewState   CapabilityReviewState   `json:"capability_review_state"`
}

type DeploymentTestStatus string

const (
	DeploymentTestHealthy   DeploymentTestStatus = "healthy"
	DeploymentTestUnhealthy DeploymentTestStatus = "unhealthy"
)

func (d *Deployment) GetRevision() uint64      { return d.Revision }
func (d *Deployment) SetRevision(value uint64) { d.Revision = value }

func (d Deployment) Validate() error {
	var problems []error
	if d.ID == "" {
		problems = append(problems, errors.New("deployment id is required"))
	}
	if strings.TrimSpace(d.Name) == "" {
		problems = append(problems, errors.New("deployment name is required"))
	}
	if d.ProviderID == "" {
		problems = append(problems, errors.New("deployment provider id is required"))
	}
	if strings.TrimSpace(d.ProviderModel) == "" {
		problems = append(problems, errors.New("deployment provider model is required"))
	}
	if d.TargetKind != "" {
		switch d.TargetKind {
		case TargetModelID, TargetAzureDeployment, TargetBedrockFoundationModel,
			TargetBedrockInferenceProfile, TargetBedrockProvisionedThroughput, TargetCustomEndpointModel:
		default:
			problems = append(problems, errors.New("deployment target kind is invalid"))
		}
	}
	if strings.TrimSpace(string(d.AccessSurface)) == "" || strings.TrimSpace(string(d.ProfileID)) == "" {
		problems = append(problems, errors.New("deployment access surface and profile are required"))
	}
	if isRegionalProfile(d.ProfileID) && strings.TrimSpace(d.Region) == "" {
		problems = append(problems, errors.New("regional deployment requires region"))
	}
	if !d.Capabilities.AnyOperation() {
		problems = append(problems, errors.New("deployment must declare at least one operation capability"))
	}
	if (d.Capabilities.Streaming || d.Capabilities.Tools || d.Capabilities.Vision || d.Capabilities.JSONMode ||
		d.Capabilities.DeveloperRole || d.Capabilities.Reasoning || d.Capabilities.StreamUsage) && !d.Capabilities.Chat {
		problems = append(problems, errors.New("deployment chat features require chat capability"))
	}
	if d.Capabilities.StreamUsage && !d.Capabilities.Streaming {
		problems = append(problems, errors.New("deployment stream usage requires streaming capability"))
	}
	if d.InputMicrosPerMillion < 0 || d.OutputMicrosPerMillion < 0 || d.FixedRequestMicrosUSD < 0 {
		problems = append(problems, errors.New("deployment prices cannot be negative"))
	}
	if d.MaxConcurrency < 0 {
		problems = append(problems, errors.New("deployment max concurrency cannot be negative"))
	}
	if d.LastTestLatencyMillis < 0 {
		problems = append(problems, errors.New("deployment test latency cannot be negative"))
	}
	if d.LastTestStatus != "" {
		if d.LastTestStatus != DeploymentTestHealthy && d.LastTestStatus != DeploymentTestUnhealthy {
			problems = append(problems, errors.New("deployment test status is invalid"))
		}
		if d.LastTestedAt == nil || d.LastTestRevision == 0 {
			problems = append(problems, errors.New("deployment test status requires timestamp and revision"))
		}
	}
	if d.Capabilities.MaxContextTokens < 0 || d.Capabilities.MaxOutputTokens < 0 ||
		(d.Capabilities.MaxContextTokens > 0 && d.Capabilities.MaxOutputTokens > d.Capabilities.MaxContextTokens) {
		problems = append(problems, errors.New("deployment capability token limits are invalid"))
	}
	if err := d.CapabilityEvidence.Validate(d.Capabilities); err != nil {
		problems = append(problems, err)
	}
	if err := d.ModelCapabilitySnapshot.Validate(d); err != nil {
		problems = append(problems, err)
	}
	if !d.CapabilityReviewState.Valid() {
		problems = append(problems, errors.New("deployment capability review state is invalid"))
	}
	if d.Priority < 0 {
		problems = append(problems, errors.New("deployment priority cannot be negative"))
	}
	if d.Weight < 0 {
		problems = append(problems, errors.New("deployment weight cannot be negative"))
	}
	return errors.Join(problems...)
}

func (c ProviderCapabilities) AnyOperation() bool {
	return c.Chat || c.Embeddings || c.Moderations || c.Images || c.Transcriptions || c.Speech || c.Files || c.Batches || c.Rerank || c.AsyncGenerate
}

func isRegionalProfile(id ProviderProfileID) bool {
	return id == ProfileBedrockConverseText || id == ProfileBedrockInvokeTitanEmbedV2 || id == ProfileBedrockInvokeTitanImageV2 || id == ProfileBedrockAgentRerankCohere35 || id == ProfileBedrockAsyncNovaReel
}

type Route struct {
	ID                     string               `json:"id"`
	PublicModel            string               `json:"public_model"`
	DeploymentID           string               `json:"deployment_id,omitempty"`
	ProviderID             string               `json:"provider_id,omitempty"` // Legacy schema v2 compatibility.
	ProviderModel          string               `json:"provider_model,omitempty"`
	InputMicrosPerMillion  int64                `json:"input_micros_per_million,omitempty"`
	OutputMicrosPerMillion int64                `json:"output_micros_per_million,omitempty"`
	Priority               int                  `json:"priority"`
	Strategy               string               `json:"strategy"`
	Enabled                bool                 `json:"enabled"`
	LastTestStatus         DeploymentTestStatus `json:"last_test_status,omitempty"`
	LastTestedAt           *time.Time           `json:"last_tested_at,omitempty"`
	LastTestLatencyMillis  int64                `json:"last_test_latency_millis,omitempty"`
	LastTestErrorClass     string               `json:"last_test_error_class,omitempty"`
	LastTestRevision       uint64               `json:"last_test_revision,omitempty"`
	CreatedAt              time.Time            `json:"created_at"`
	UpdatedAt              time.Time            `json:"updated_at"`
	Revision               uint64               `json:"revision"`
	DeletedAt              *time.Time           `json:"deleted_at,omitempty"`
}

func (r *Route) GetRevision() uint64      { return r.Revision }
func (r *Route) SetRevision(value uint64) { r.Revision = value }

func (r Route) Validate() error {
	var problems []error
	if r.ID == "" {
		problems = append(problems, errors.New("route id is required"))
	}
	if strings.TrimSpace(r.PublicModel) == "" {
		problems = append(problems, errors.New("route public model is required"))
	}
	if r.DeploymentID == "" {
		if r.ProviderID == "" {
			problems = append(problems, errors.New("route deployment id is required"))
		}
		if strings.TrimSpace(r.ProviderModel) == "" {
			problems = append(problems, errors.New("legacy route provider model is required"))
		}
	}
	if r.InputMicrosPerMillion < 0 || r.OutputMicrosPerMillion < 0 {
		problems = append(problems, errors.New("route prices cannot be negative"))
	}
	if r.Priority < 0 {
		problems = append(problems, errors.New("route priority cannot be negative"))
	}
	if r.Strategy != "" && r.Strategy != "ordered" && r.Strategy != "round_robin" {
		problems = append(problems, errors.New("route strategy must be ordered or round_robin"))
	}
	if r.LastTestLatencyMillis < 0 {
		problems = append(problems, errors.New("route test latency cannot be negative"))
	}
	if r.LastTestStatus != "" {
		if r.LastTestStatus != DeploymentTestHealthy && r.LastTestStatus != DeploymentTestUnhealthy {
			problems = append(problems, errors.New("route test status is invalid"))
		}
		if r.LastTestedAt == nil || r.LastTestRevision == 0 {
			problems = append(problems, errors.New("route test status requires timestamp and revision"))
		}
	}
	return errors.Join(problems...)
}

type TokenGuardPolicy struct {
	ID                              string        `json:"id"`
	Name                            string        `json:"name"`
	Enabled                         bool          `json:"enabled"`
	Action                          string        `json:"action"`
	RequestTokens                   int64         `json:"request_tokens"`
	TokensPerMinute                 int64         `json:"tokens_per_minute"`
	CostMicrosPerMinute             int64         `json:"cost_micros_per_minute"`
	ErrorRate                       float64       `json:"error_rate"`
	MinimumSamples                  int64         `json:"minimum_samples"`
	Concurrency                     int64         `json:"concurrency"`
	UniqueIPsPerMinute              int64         `json:"unique_ips_per_minute"`
	ViolationsBeforeBlock           int64         `json:"violations_before_block"`
	BlockTTL                        time.Duration `json:"block_ttl"`
	Cooldown                        time.Duration `json:"cooldown"`
	EWMAEnabled                     bool          `json:"ewma_enabled"`
	EWMAAlpha                       float64       `json:"ewma_alpha"`
	EWMAMultiplier                  float64       `json:"ewma_multiplier"`
	EWMAMinimumSamples              int64         `json:"ewma_minimum_samples"`
	EWMAWarmup                      time.Duration `json:"ewma_warmup"`
	EWMAEvaluationWindow            time.Duration `json:"ewma_evaluation_window"`
	EWMACooldown                    time.Duration `json:"ewma_cooldown"`
	EWMAAbsoluteRPM                 int64         `json:"ewma_absolute_rpm"`
	EWMAAbsoluteTPM                 int64         `json:"ewma_absolute_tpm"`
	EWMAAbsoluteTokensPerRequest    float64       `json:"ewma_absolute_tokens_per_request"`
	EWMAAbsoluteCostMicrosPerMinute int64         `json:"ewma_absolute_cost_micros_per_minute"`
	CreatedAt                       time.Time     `json:"created_at"`
	UpdatedAt                       time.Time     `json:"updated_at"`
	Revision                        uint64        `json:"revision"`
	DeletedAt                       *time.Time    `json:"deleted_at,omitempty"`
}

type AlertWebhook struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	URL          string     `json:"url"`
	AllowedHosts []string   `json:"allowed_hosts"`
	HeaderName   string     `json:"header_name"`
	CredentialID string     `json:"credential_id,omitempty"`
	Enabled      bool       `json:"enabled"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	Revision     uint64     `json:"revision"`
	DeletedAt    *time.Time `json:"deleted_at,omitempty"`
}

func (w *AlertWebhook) GetRevision() uint64      { return w.Revision }
func (w *AlertWebhook) SetRevision(value uint64) { w.Revision = value }

func (w AlertWebhook) Validate() error {
	var problems []error
	if w.ID == "" {
		problems = append(problems, errors.New("alert webhook id is required"))
	}
	if strings.TrimSpace(w.Name) == "" {
		problems = append(problems, errors.New("alert webhook name is required"))
	}
	if strings.TrimSpace(w.URL) == "" {
		problems = append(problems, errors.New("alert webhook URL is required"))
	}
	if len(w.AllowedHosts) == 0 {
		problems = append(problems, errors.New("alert webhook allowed hosts must not be empty"))
	}
	if w.CredentialID != "" {
		switch strings.ToLower(w.HeaderName) {
		case "authorization", "x-webhook-token":
		default:
			problems = append(problems, errors.New("alert webhook secret header is not allowed"))
		}
	} else if w.HeaderName != "" {
		problems = append(problems, errors.New("alert webhook header requires a credential"))
	}
	return errors.Join(problems...)
}

func (p *TokenGuardPolicy) GetRevision() uint64      { return p.Revision }
func (p *TokenGuardPolicy) SetRevision(value uint64) { p.Revision = value }

func (p TokenGuardPolicy) Validate() error {
	var problems []error
	if p.ID == "" {
		problems = append(problems, errors.New("token guard policy id is required"))
	}
	if strings.TrimSpace(p.Name) == "" {
		problems = append(problems, errors.New("token guard policy name is required"))
	}
	switch p.Action {
	case "observe", "alert", "temporary_block":
	default:
		problems = append(problems, errors.New("token guard action must be observe, alert, or temporary_block"))
	}
	for name, value := range map[string]int64{
		"request_tokens": p.RequestTokens, "tokens_per_minute": p.TokensPerMinute,
		"cost_micros_per_minute": p.CostMicrosPerMinute, "minimum_samples": p.MinimumSamples,
		"concurrency": p.Concurrency, "unique_ips_per_minute": p.UniqueIPsPerMinute,
		"violations_before_block": p.ViolationsBeforeBlock,
	} {
		if value < 0 {
			problems = append(problems, errors.New(name+" cannot be negative"))
		}
	}
	if p.ErrorRate < 0 || p.ErrorRate > 1 {
		problems = append(problems, errors.New("error_rate must be between 0 and 1"))
	}
	if p.Action == "temporary_block" {
		if p.ViolationsBeforeBlock < 2 {
			problems = append(problems, errors.New("temporary block requires at least two violations"))
		}
		if p.BlockTTL <= 0 || p.Cooldown <= 0 {
			problems = append(problems, errors.New("temporary block TTL and cooldown must be positive"))
		}
	}
	if p.EWMAEnabled {
		if p.EWMAAlpha <= 0 || p.EWMAAlpha > 1 {
			problems = append(problems, errors.New("ewma_alpha must be greater than 0 and at most 1"))
		}
		if p.EWMAMultiplier <= 1 {
			problems = append(problems, errors.New("ewma_multiplier must be greater than 1"))
		}
		if p.EWMAMinimumSamples < 10 {
			problems = append(problems, errors.New("ewma_minimum_samples must be at least 10"))
		}
		if p.EWMAEvaluationWindow < 10*time.Second || p.EWMAEvaluationWindow > 5*time.Minute ||
			p.EWMAEvaluationWindow%(10*time.Second) != 0 {
			problems = append(problems, errors.New("ewma_evaluation_window must be a 10-second multiple between 10 seconds and 5 minutes"))
		}
		if p.EWMAWarmup < p.EWMAEvaluationWindow || p.EWMACooldown <= 0 {
			problems = append(problems, errors.New("ewma_warmup must cover an evaluation window and ewma_cooldown must be positive"))
		}
		if p.EWMAAbsoluteRPM < 0 || p.EWMAAbsoluteTPM < 0 || p.EWMAAbsoluteTokensPerRequest < 0 || p.EWMAAbsoluteCostMicrosPerMinute < 0 {
			problems = append(problems, errors.New("EWMA absolute floors cannot be negative"))
		}
		if p.EWMAAbsoluteRPM == 0 && p.EWMAAbsoluteTPM == 0 && p.EWMAAbsoluteTokensPerRequest == 0 && p.EWMAAbsoluteCostMicrosPerMinute == 0 {
			problems = append(problems, errors.New("EWMA requires at least one absolute floor"))
		}
	}
	return errors.Join(problems...)
}

func (k *GatewayKey) GetRevision() uint64      { return k.Revision }
func (k *GatewayKey) SetRevision(value uint64) { k.Revision = value }

func (k GatewayKey) Validate() error {
	var problems []error
	if k.ID == "" {
		problems = append(problems, errors.New("gateway key id is required"))
	}
	if k.ProjectID == "" {
		problems = append(problems, errors.New("gateway key project id is required"))
	}
	if strings.TrimSpace(k.Name) == "" {
		problems = append(problems, errors.New("gateway key name is required"))
	}
	if k.HashVersion != 1 {
		problems = append(problems, errors.New("unsupported gateway key hash version"))
	}
	return errors.Join(problems...)
}
