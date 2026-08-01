package domain

import "testing"

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
