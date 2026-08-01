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
