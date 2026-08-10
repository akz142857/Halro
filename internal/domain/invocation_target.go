package domain

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// AvailabilityState describes whether the current provider instance can see an
// invocation target. Availability is deliberately independent from capability
// evidence: appearing in a provider catalog never proves what a target can do.
type AvailabilityState string

const (
	AvailabilityAvailable   AvailabilityState = "available"
	AvailabilityUnverified  AvailabilityState = "unverified"
	AvailabilityUnavailable AvailabilityState = "unavailable"
)

type TargetLifecycle string

const (
	TargetLifecycleActive     TargetLifecycle = "active"
	TargetLifecycleDeprecated TargetLifecycle = "deprecated"
	TargetLifecycleUnknown    TargetLifecycle = "unknown"
)

type MetadataSource string

const (
	MetadataSourceNone     MetadataSource = "none"
	MetadataSourceProvider MetadataSource = "provider_metadata"
)

// NormalizedModelMetadata is the allowlisted, provider-neutral subset of an
// upstream catalog response. Raw provider JSON is never retained here.
type NormalizedModelMetadata struct {
	InputModalities     []string `json:"input_modalities,omitempty"`
	OutputModalities    []string `json:"output_modalities,omitempty"`
	SupportedOperations []string `json:"supported_operations,omitempty"`
	InferenceTypes      []string `json:"inference_types,omitempty"`
	MaxContextTokens    int64    `json:"max_context_tokens,omitempty"`
	MaxOutputTokens     int64    `json:"max_output_tokens,omitempty"`
}

// InvocationTargetScopeKey prevents evidence from leaking across provider
// instances, target identities, bindings, or location semantics.
type InvocationTargetScopeKey struct {
	ProviderID string               `json:"provider_id"`
	TargetKind DeploymentTargetKind `json:"target_kind"`
	TargetID   string               `json:"target_id"`
	BindingID  string               `json:"binding_id"`
	ProfileID  ProviderProfileID    `json:"profile_id"`
	Location   string               `json:"location,omitempty"`
}

func (s InvocationTargetScopeKey) Validate() error {
	if strings.TrimSpace(s.ProviderID) == "" || strings.TrimSpace(s.TargetID) == "" ||
		strings.TrimSpace(s.BindingID) == "" || strings.TrimSpace(string(s.ProfileID)) == "" || s.TargetKind == "" {
		return errors.New("capability claim scope requires provider, target, binding, profile, and target kind")
	}
	return nil
}

type InvocationTargetDescriptor struct {
	TargetID          string                  `json:"target_id"`
	TargetKind        DeploymentTargetKind    `json:"target_kind"`
	DisplayName       string                  `json:"display_name"`
	OwnedBy           string                  `json:"owned_by,omitempty"`
	CanonicalModelRef string                  `json:"canonical_model_ref,omitempty"`
	Region            string                  `json:"region,omitempty"`
	Lifecycle         TargetLifecycle         `json:"lifecycle"`
	Metadata          NormalizedModelMetadata `json:"metadata"`
	MetadataSource    MetadataSource          `json:"metadata_source"`
	Availability      AvailabilityState       `json:"availability"`
	FetchedAt         time.Time               `json:"fetched_at"`
}

type TargetQuery struct {
	TargetKind DeploymentTargetKind `json:"target_kind,omitempty"`
	Region     string               `json:"region,omitempty"`
}

// InvocationTargetDiscoveryCapabilities lets the Admin UI render the target
// workflow from adapter facts instead of a provider-type switch statement.
type InvocationTargetDiscoveryCapabilities struct {
	TargetKinds                   []DeploymentTargetKind `json:"target_kinds"`
	CanEnumerate                  bool                   `json:"can_enumerate"`
	CanDescribe                   bool                   `json:"can_describe"`
	CanVerify                     bool                   `json:"can_verify"`
	RequiresManagementIdentity    bool                   `json:"requires_management_identity"`
	RequiresCanonicalModelMapping bool                   `json:"requires_canonical_model_mapping"`
}

type ClaimStatus string

const (
	ClaimSupported   ClaimStatus = "supported"
	ClaimUnsupported ClaimStatus = "unsupported"
	ClaimUnknown     ClaimStatus = "unknown"
	ClaimConflicting ClaimStatus = "conflicting"
)

type ClaimSource string

const (
	ClaimSourceBuiltinCatalog   ClaimSource = "builtin_catalog"
	ClaimSourceProviderMetadata ClaimSource = "provider_metadata"
	ClaimSourceSignedCatalog    ClaimSource = "signed_catalog"
	ClaimSourceVerifiedProbe    ClaimSource = "verified_probe"
	ClaimSourceOperatorDeclared ClaimSource = "operator_declared"
)

type CapabilityClaim struct {
	CapabilityID string                   `json:"capability_id"`
	Status       ClaimStatus              `json:"status"`
	Evidence     CapabilityEvidence       `json:"evidence"`
	Source       ClaimSource              `json:"source"`
	Scope        InvocationTargetScopeKey `json:"scope"`
	ObservedAt   time.Time                `json:"observed_at"`
	ExpiresAt    *time.Time               `json:"expires_at,omitempty"`
	Revision     string                   `json:"revision"`
}

func (c CapabilityClaim) Validate() error {
	if !slices.Contains(capabilityNames, c.CapabilityID) {
		return fmt.Errorf("capability claim names unknown capability %q", c.CapabilityID)
	}
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if c.ObservedAt.IsZero() || strings.TrimSpace(c.Revision) == "" {
		return errors.New("capability claim requires observation time and revision")
	}
	if c.ExpiresAt != nil && !c.ExpiresAt.After(c.ObservedAt) {
		return errors.New("capability claim expiry must follow observation time")
	}
	switch c.Status {
	case ClaimSupported, ClaimUnsupported, ClaimUnknown, ClaimConflicting:
	default:
		return errors.New("capability claim status is invalid")
	}
	switch c.Source {
	case ClaimSourceBuiltinCatalog, ClaimSourceProviderMetadata, ClaimSourceSignedCatalog,
		ClaimSourceVerifiedProbe, ClaimSourceOperatorDeclared:
	default:
		return errors.New("capability claim source is invalid")
	}
	switch c.Evidence {
	case EvidenceVerified, EvidenceDeclared, EvidenceUnsupported:
	default:
		return errors.New("capability claim evidence is invalid")
	}
	if evidenceRankValue(c.Evidence) > evidenceRankValue(MaxEvidenceForCapabilitySource(string(c.Source))) {
		return fmt.Errorf("capability claim evidence exceeds what source %q may establish", c.Source)
	}
	if (c.Source == ClaimSourceProviderMetadata || c.Source == ClaimSourceVerifiedProbe) && c.ExpiresAt == nil {
		return fmt.Errorf("capability claim source %q requires an expiry", c.Source)
	}
	if c.Source == ClaimSourceOperatorDeclared && (c.Status != ClaimSupported || c.Evidence != EvidenceDeclared) {
		return errors.New("operator declaration may only establish declared support")
	}
	switch c.Status {
	case ClaimSupported:
		if c.Evidence == EvidenceUnsupported {
			return errors.New("supported capability claim requires positive evidence")
		}
	case ClaimUnsupported:
		if c.Evidence == EvidenceUnsupported {
			return errors.New("unsupported capability claim requires explicit negative evidence")
		}
	case ClaimUnknown:
		if c.Evidence != EvidenceUnsupported {
			return errors.New("unknown capability claim cannot carry positive evidence")
		}
	}
	return nil
}

// ActiveAt keeps expiring provider/probe evidence out of new resolutions.
// Existing deployments retain their immutable snapshot and are never silently
// widened or narrowed when a claim expires.
func (c CapabilityClaim) ActiveAt(at time.Time) bool {
	return c.ExpiresAt == nil || at.Before(*c.ExpiresAt)
}

type ResolutionState string

const (
	ResolutionResolved    ResolutionState = "resolved"
	ResolutionUnknown     ResolutionState = "unknown"
	ResolutionConflicting ResolutionState = "conflicting"
	ResolutionNoVariant   ResolutionState = "no_variant"
)

type DeploymentVariant struct {
	ID               string                     `json:"id"`
	BindingID        string                     `json:"binding_id"`
	ProfileID        ProviderProfileID          `json:"profile_id"`
	Target           InvocationTargetDescriptor `json:"target"`
	Capabilities     ProviderCapabilities       `json:"capabilities"`
	CapabilityClaims []CapabilityClaim          `json:"capability_claims"`
	ResolutionState  ResolutionState            `json:"resolution_state"`
	Revision         string                     `json:"revision"`
}

func (v DeploymentVariant) Validate(binding ProviderProfileBinding) error {
	providerID := ""
	if len(v.CapabilityClaims) > 0 {
		providerID = v.CapabilityClaims[0].Scope.ProviderID
	}
	return v.ValidateForProvider(providerID, binding)
}

// ValidateForProvider checks the complete resolver scope. The provider ID is a
// caller-owned fact rather than a field on the target descriptor, so resolver
// boundaries should use this method instead of trusting the first claim to
// establish it.
func (v DeploymentVariant) ValidateForProvider(providerID string, binding ProviderProfileBinding) error {
	if strings.TrimSpace(v.ID) == "" || strings.TrimSpace(v.Revision) == "" {
		return errors.New("deployment variant requires id and revision")
	}
	if strings.TrimSpace(providerID) == "" {
		return errors.New("deployment variant requires an exact provider scope")
	}
	if v.BindingID != binding.ID || v.ProfileID != binding.ProfileID {
		return errors.New("deployment variant must bind exactly one matching provider binding")
	}
	if !ProviderCapabilitiesSubset(v.Capabilities, binding.Capabilities) {
		return errors.New("deployment variant capabilities exceed binding ceiling")
	}
	if v.ResolutionState == ResolutionResolved && !v.Capabilities.AnyOperation() {
		return errors.New("resolved deployment variant requires an operation capability")
	}
	for _, claim := range v.CapabilityClaims {
		if claim.Scope.ProviderID != providerID || claim.Scope.BindingID != v.BindingID || claim.Scope.ProfileID != v.ProfileID ||
			claim.Scope.TargetID != v.Target.TargetID || claim.Scope.TargetKind != v.Target.TargetKind ||
			claim.Scope.Location != v.Target.Region {
			return errors.New("deployment variant claim escapes target or binding scope")
		}
		if err := claim.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// CapabilityNames returns the stable dictionary order for resolution and API
// projections without exposing the package's mutable backing slice.
func CapabilityNames() []string { return slices.Clone(capabilityNames) }
