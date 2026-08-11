package domain

import (
	"strings"
	"testing"
)

func TestBedrockMantleCredentialProfileResolution(t *testing.T) {
	profile, ok := ResolveCredentialProfile(ProviderBedrock, SurfaceBedrockMantle, CredentialBedrockAPIKey)
	if !ok || profile.AccessSurface != SurfaceBedrockMantle || profile.CredentialScheme != CredentialBedrockAPIKey {
		t.Fatalf("Mantle credential profile resolution failed: %#v ok=%v", profile, ok)
	}
	if _, ok := ResolveCredentialProfile(ProviderBedrock, SurfaceBedrockMantle, CredentialAWSSigV4Explicit); ok {
		t.Fatal("Mantle accepted a Runtime credential scheme")
	}
}

func TestCredentialValidationStillRequiresExplicitProfileAxes(t *testing.T) {
	credential := Credential{ID: "cred_1", Name: "test", Type: ProviderBedrock, Audience: "https://example", Ciphertext: []byte("ciphertext")}
	if err := credential.Validate(); err == nil {
		t.Fatal("credential without explicit surface and scheme was accepted")
	}
}

func TestBedrockRuntimeCredentialCanServeRegisteredInvokeProfiles(t *testing.T) {
	profile, ok := ResolveProviderProfile(ProviderBedrock, ProfileBedrockInvokeTitanEmbedV2)
	if !ok || profile.AccessSurface != SurfaceBedrockRuntime || profile.CredentialScheme != CredentialAWSSigV4Explicit {
		t.Fatalf("Invoke profile resolution failed: %#v ok=%v", profile, ok)
	}
	credential, ok := ResolveCredentialProfile(ProviderBedrock, SurfaceBedrockRuntime, CredentialAWSSigV4Explicit)
	if !ok || credential.AccessSurface != SurfaceBedrockRuntime {
		t.Fatalf("Runtime credential resolution failed: %#v ok=%v", credential, ok)
	}
}

func TestBedrockAgentRuntimeIsOnlyResolvedForRegisteredRerankProfile(t *testing.T) {
	profile, ok := ResolveProviderProfile(ProviderBedrock, ProfileBedrockAgentRerankCohere35)
	if !ok || profile.AccessSurface != SurfaceBedrockAgentRuntime || profile.CredentialScheme != CredentialAWSSigV4Explicit {
		t.Fatalf("rerank profile resolution failed: %#v ok=%v", profile, ok)
	}
	if _, ok := ResolveCredentialProfile(ProviderBedrock, SurfaceBedrockAgentRuntime, CredentialBedrockAPIKey); ok {
		t.Fatal("agent runtime accepted the Mantle credential scheme")
	}
}

func TestProviderCapabilitiesSubsetCoversEveryOperation(t *testing.T) {
	available := ProviderCapabilities{}
	cases := []ProviderCapabilities{
		{Chat: true}, {Embeddings: true}, {Moderations: true}, {Images: true},
		{Transcriptions: true}, {Speech: true}, {Files: true}, {Batches: true},
		{Rerank: true}, {AsyncGenerate: true},
	}
	for _, candidate := range cases {
		if ProviderCapabilitiesSubset(candidate, available) {
			t.Fatalf("unsupported operation was accepted: %#v", candidate)
		}
	}
}

func TestDeploymentValidationRejectsDetachedChatFeatures(t *testing.T) {
	deployment := Deployment{
		ID: "dep_1", Name: "invalid", ProviderID: "prv_1", ProviderModel: "model",
		AccessSurface: SurfaceOpenAI, ProfileID: ProfileOpenAIChatEmbeddings,
		Capabilities:       ProviderCapabilities{Embeddings: true, Tools: true},
		CapabilityEvidence: EvidenceForCapabilities(ProviderCapabilities{Embeddings: true, Tools: true}, EvidenceDeclared),
	}
	if err := deployment.Validate(); err == nil || !strings.Contains(err.Error(), "chat features require chat") {
		t.Fatalf("detached chat feature accepted: %v", err)
	}
}

func TestProviderProfileBindingIdentityAndLegacyProjection(t *testing.T) {
	capabilities := DefaultProviderCapabilitiesForProfile(ProviderOpenAI, ProfileOpenAIChatEmbeddings)
	evidence := EvidenceForCapabilities(capabilities, EvidenceDeclared)
	provider := ProviderInstance{
		ID: "prv_1", Type: ProviderOpenAI, ProfileID: ProfileOpenAIChatEmbeddings,
		AccessSurface: SurfaceOpenAI, CredentialScheme: CredentialBearerStatic,
		Capabilities: capabilities, CapabilityEvidence: evidence,
	}
	bindings := provider.EffectiveProfileBindings()
	if len(bindings) != 1 || bindings[0].ProviderID != provider.ID ||
		bindings[0].ID != DefaultProviderProfileBindingID(provider.ID, provider.ProfileID) {
		t.Fatalf("legacy binding projection is unstable: %#v", bindings)
	}
}

func TestBindingsCapabilitiesSummaryUnionsEnabledProfiles(t *testing.T) {
	chat := DefaultProviderCapabilitiesForProfile(ProviderOpenAI, ProfileOpenAIChatEmbeddings)
	media := DefaultProviderCapabilitiesForProfile(ProviderOpenAI, ProfileOpenAIMediaResources)
	bindings := []ProviderProfileBinding{
		{Enabled: true, Capabilities: chat, CapabilityEvidence: EvidenceForCapabilities(chat, EvidenceDeclared)},
		{Enabled: true, Capabilities: media, CapabilityEvidence: EvidenceForCapabilities(media, EvidenceDeclared)},
	}
	summary, evidence := BindingsCapabilitiesSummary(bindings)
	if !summary.Chat || !summary.Embeddings || !summary.Images || !summary.Files || evidence["images"] != EvidenceDeclared {
		t.Fatalf("binding summary lost operations: %#v %#v", summary, evidence)
	}
}

func TestProviderBindingValidationRejectsDuplicateProfileAndMixedSurface(t *testing.T) {
	chat := DefaultProviderCapabilitiesForProfile(ProviderOpenAI, ProfileOpenAIChatEmbeddings)
	evidence := EvidenceForCapabilities(chat, EvidenceDeclared)
	provider := validBoundProviderForTest(chat, evidence)
	duplicate := provider.Bindings[0]
	duplicate.ID = "different-id"
	provider.Bindings = append(provider.Bindings, duplicate)
	if err := provider.Validate(); err == nil || !strings.Contains(err.Error(), "must not repeat") {
		t.Fatalf("duplicate profile was accepted: %v", err)
	}
	provider = validBoundProviderForTest(chat, evidence)
	provider.Bindings[0].AccessSurface = SurfaceBedrockRuntime
	if err := provider.Validate(); err == nil || !strings.Contains(err.Error(), "access surface") {
		t.Fatalf("mixed surface was accepted: %v", err)
	}
}

func TestMediaOnlyProviderAllowsDisabledZeroCapabilityBindingAndRejectsAllDisabled(t *testing.T) {
	chatZero := ProviderCapabilities{}
	media := DefaultProviderCapabilitiesForProfile(ProviderOpenAI, ProfileOpenAIMediaResources)
	provider := validBoundProviderForTest(media, EvidenceForCapabilities(media, EvidenceDeclared))
	provider.ProfileID = ProfileOpenAIMediaResources
	provider.Bindings = []ProviderProfileBinding{
		{ID: DefaultProviderProfileBindingID(provider.ID, ProfileOpenAIMediaResources), ProviderID: provider.ID, ProfileID: ProfileOpenAIMediaResources, AccessSurface: SurfaceOpenAI, CredentialScheme: CredentialBearerStatic, Capabilities: media, CapabilityEvidence: EvidenceForCapabilities(media, EvidenceDeclared), Enabled: true},
		{ID: DefaultProviderProfileBindingID(provider.ID, ProfileOpenAIChatEmbeddings), ProviderID: provider.ID, ProfileID: ProfileOpenAIChatEmbeddings, AccessSurface: SurfaceOpenAI, CredentialScheme: CredentialBearerStatic, Capabilities: chatZero, CapabilityEvidence: EvidenceForCapabilities(chatZero, EvidenceDeclared), Enabled: false},
	}
	provider.Capabilities, provider.CapabilityEvidence = BindingsCapabilitiesSummary(provider.Bindings)
	if err := provider.Validate(); err != nil {
		t.Fatalf("media-only provider rejected: %v", err)
	}
	provider.Bindings[0].Enabled = false
	provider.Capabilities, provider.CapabilityEvidence = BindingsCapabilitiesSummary(provider.Bindings)
	if err := provider.Validate(); err == nil || !strings.Contains(err.Error(), "enabled profile binding") {
		t.Fatalf("all-disabled provider accepted: %v", err)
	}
}

func validBoundProviderForTest(capabilities ProviderCapabilities, evidence CapabilityEvidenceSet) ProviderInstance {
	p := ProviderInstance{ID: "prv_1", Name: "OpenAI", Type: ProviderOpenAI, BaseURL: "https://api.openai.com", CredentialID: "cred_1", AccessSurface: SurfaceOpenAI, ProfileID: ProfileOpenAIChatEmbeddings, CredentialScheme: CredentialBearerStatic, AllowedHosts: []string{"api.openai.com"}, Capabilities: capabilities, CapabilityEvidence: evidence, Enabled: true}
	p.Bindings = []ProviderProfileBinding{{ID: DefaultProviderProfileBindingID(p.ID, p.ProfileID), ProviderID: p.ID, ProfileID: p.ProfileID, AccessSurface: p.AccessSurface, CredentialScheme: p.CredentialScheme, Capabilities: capabilities, CapabilityEvidence: evidence.Clone(), Enabled: true}}
	return p
}
