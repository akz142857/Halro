package domain

import (
	"testing"
	"time"
)

func TestCapabilityClaimValidationAndExpiry(t *testing.T) {
	observed := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	expires := observed.Add(5 * time.Minute)
	claim := CapabilityClaim{
		CapabilityID: "chat", Status: ClaimSupported, Evidence: EvidenceDeclared,
		Source: ClaimSourceProviderMetadata,
		Scope: InvocationTargetScopeKey{
			ProviderID: "provider", TargetKind: TargetModelID, TargetID: "model",
			BindingID: "binding", ProfileID: ProfileOpenAIChatEmbeddings,
		},
		ObservedAt: observed, ExpiresAt: &expires, Revision: "sha256:claim",
	}
	if err := claim.Validate(); err != nil {
		t.Fatal(err)
	}
	if !claim.ActiveAt(expires.Add(-time.Nanosecond)) || claim.ActiveAt(expires) {
		t.Fatal("claim expiry boundary is not fail-closed")
	}
	claim.Source = ClaimSourceSignedCatalog
	claim.ExpiresAt = nil
	if err := claim.Validate(); err != nil || !claim.ActiveAt(expires.Add(24*time.Hour)) {
		t.Fatalf("reserved signed catalog source is invalid: %v", err)
	}
}

func TestCapabilityClaimRejectsEvidenceThatItsSourceOrStatusCannotEstablish(t *testing.T) {
	expires := time.Now().UTC().Add(time.Minute)
	base := CapabilityClaim{
		CapabilityID: "chat", Status: ClaimSupported, Evidence: EvidenceDeclared,
		Source: ClaimSourceProviderMetadata,
		Scope: InvocationTargetScopeKey{
			ProviderID: "provider", TargetKind: TargetModelID, TargetID: "model",
			BindingID: "binding", ProfileID: ProfileOpenAIChatEmbeddings,
		},
		ObservedAt: time.Now().UTC(), ExpiresAt: &expires, Revision: "sha256:claim",
	}
	tests := []struct {
		name   string
		mutate func(*CapabilityClaim)
	}{
		{name: "provider metadata cannot verify", mutate: func(claim *CapabilityClaim) { claim.Evidence = EvidenceVerified }},
		{name: "supported needs positive evidence", mutate: func(claim *CapabilityClaim) { claim.Evidence = EvidenceUnsupported }},
		{name: "unknown cannot carry positive evidence", mutate: func(claim *CapabilityClaim) { claim.Status = ClaimUnknown }},
		{name: "provider metadata requires expiry", mutate: func(claim *CapabilityClaim) { claim.ExpiresAt = nil }},
		{name: "operator cannot establish unsupported", mutate: func(claim *CapabilityClaim) {
			claim.Source, claim.Status = ClaimSourceOperatorDeclared, ClaimUnsupported
		}},
		{name: "invalid evidence", mutate: func(claim *CapabilityClaim) { claim.Evidence = CapabilityEvidence("future") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claim := base
			test.mutate(&claim)
			if err := claim.Validate(); err == nil {
				t.Fatal("invalid source/status/evidence combination was accepted")
			}
		})
	}
}

func TestDeploymentVariantRejectsCrossBindingClaimsAndCapabilityWidening(t *testing.T) {
	binding := ProviderProfileBinding{
		ID: "binding", ProfileID: ProfileOpenAIChatEmbeddings,
		Capabilities: ProviderCapabilities{Chat: true},
	}
	variant := DeploymentVariant{
		ID: "variant", BindingID: binding.ID, ProfileID: binding.ProfileID,
		Target:       InvocationTargetDescriptor{TargetID: "model", TargetKind: TargetModelID},
		Capabilities: ProviderCapabilities{Chat: true}, ResolutionState: ResolutionResolved, Revision: "sha256:variant",
		CapabilityClaims: []CapabilityClaim{{
			CapabilityID: "chat", Status: ClaimSupported, Evidence: EvidenceDeclared,
			Source: ClaimSourceBuiltinCatalog, ObservedAt: time.Now().UTC(), Revision: "sha256:claim",
			Scope: InvocationTargetScopeKey{ProviderID: "provider", TargetKind: TargetModelID, TargetID: "model", BindingID: "other", ProfileID: binding.ProfileID},
		}},
	}
	if err := variant.Validate(binding); err == nil {
		t.Fatal("cross-binding claim was accepted")
	}
	variant.CapabilityClaims[0].Scope.BindingID = binding.ID
	variant.Capabilities.Tools = true
	if err := variant.Validate(binding); err == nil {
		t.Fatal("capability widening beyond binding ceiling was accepted")
	}
}

func TestDeploymentVariantRejectsProviderAndLocationScopeEscape(t *testing.T) {
	binding := ProviderProfileBinding{
		ID: "binding", ProfileID: ProfileOpenAIChatEmbeddings,
		Capabilities: ProviderCapabilities{Chat: true},
	}
	variant := DeploymentVariant{
		ID: "variant", BindingID: binding.ID, ProfileID: binding.ProfileID,
		Target:       InvocationTargetDescriptor{TargetID: "model", TargetKind: TargetModelID, Region: "region-a"},
		Capabilities: ProviderCapabilities{Chat: true}, ResolutionState: ResolutionResolved, Revision: "sha256:variant",
		CapabilityClaims: []CapabilityClaim{{
			CapabilityID: "chat", Status: ClaimSupported, Evidence: EvidenceDeclared,
			Source: ClaimSourceBuiltinCatalog, ObservedAt: time.Now().UTC(), Revision: "sha256:claim",
			Scope: InvocationTargetScopeKey{ProviderID: "provider-a", TargetKind: TargetModelID, TargetID: "model", BindingID: binding.ID, ProfileID: binding.ProfileID, Location: "region-a"},
		}},
	}
	if err := variant.ValidateForProvider("provider-a", binding); err != nil {
		t.Fatal(err)
	}
	if err := variant.ValidateForProvider("provider-b", binding); err == nil {
		t.Fatal("cross-provider claim was accepted")
	}
	variant.CapabilityClaims[0].Scope.Location = "region-b"
	if err := variant.ValidateForProvider("provider-a", binding); err == nil {
		t.Fatal("cross-location claim was accepted")
	}
}
