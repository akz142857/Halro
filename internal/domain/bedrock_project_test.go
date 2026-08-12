package domain

import (
	"strings"
	"testing"
)

func TestBedrockProjectIDAcceptsOnlyAWSProjectIdentifiers(t *testing.T) {
	for _, value := range []string{"", "proj_5d5ykleja6cwpirysbb7", "proj_ABC123"} {
		if err := ValidateBedrockProjectID(value); err != nil {
			t.Fatalf("rejected a valid project id %q: %v", value, err)
		}
	}
	for _, value := range []string{
		"proj_", "project_1", "5d5ykleja6cwpirysbb7", "proj_has-dash", "proj_has_underscore",
		"proj_a b", "proj_a\nb", "default", strings.Repeat("proj_a", 40),
	} {
		if err := ValidateBedrockProjectID(value); err == nil {
			t.Fatalf("accepted %q as a Bedrock project id", value)
		}
	}
	// At the bound, not near it: the only over-length value above also carries
	// underscores, so the alphanumeric rule refused it and the length rule was
	// never the thing under test.
	atLimit := "proj_" + strings.Repeat("a", MaxBedrockProjectIDLength-len("proj_"))
	if err := ValidateBedrockProjectID(atLimit); err != nil {
		t.Fatalf("rejected a project id of exactly the maximum length: %v", err)
	}
	if err := ValidateBedrockProjectID(atLimit + "a"); err == nil {
		t.Fatal("accepted a project id one character over the maximum length")
	}
}

// Pasting a Claude Platform on AWS workspace id into the Bedrock field is the
// likeliest mistake here: the two products use similar words for a similar
// concept, on different hosts, with different header names. The refusal says
// which product the value came from rather than "invalid format".
func TestBedrockProjectIDNamesTheClaudePlatformMistake(t *testing.T) {
	err := ValidateBedrockProjectID("wrkspc_01AbCdEf23GhIj")
	if err == nil {
		t.Fatal("a Claude Platform workspace id was accepted as a Bedrock project id")
	}
	if !strings.Contains(err.Error(), "Claude Platform") {
		t.Fatalf("the refusal does not say where the value came from: %v", err)
	}
}

// `default` is the ID AWS lists for the account default project. Storing it
// would give "the default project" two spellings, and sending it as a header
// does nothing a missing header does not already do.
func TestBedrockProjectIDNormalizesTheDefaultProjectToEmpty(t *testing.T) {
	for _, value := range []string{"default", " default ", ""} {
		if got := NormalizeBedrockProjectID(value); got != "" {
			t.Fatalf("%q normalised to %q rather than the empty default", value, got)
		}
	}
	if got := NormalizeBedrockProjectID(" proj_abc "); got != "proj_abc" {
		t.Fatalf("a padded project id normalised to %q", got)
	}
}

// The field is Mantle-only. On any other surface it would be stored and never
// sent — a setting that silently does nothing, which is worse than a refusal.
func TestBedrockProjectIDIsRefusedOutsideTheMantleSurface(t *testing.T) {
	instance := ProviderInstance{
		ID: "prov_1", Name: "runtime", Type: ProviderBedrock,
		BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com", CredentialID: "cred_1",
		AccessSurface: SurfaceBedrockRuntime, ProfileID: ProfileBedrockConverseText,
		CredentialScheme: CredentialAWSSigV4Explicit,
		AllowedHosts:     []string{"bedrock-runtime.us-east-1.amazonaws.com"},
		BedrockProjectID: "proj_abc123", Enabled: true,
		Capabilities: ProviderCapabilities{Chat: true, Streaming: true},
	}
	instance.CapabilityEvidence = EvidenceForCapabilities(instance.Capabilities, EvidenceDeclared)
	err := instance.Validate()
	if err == nil || !strings.Contains(err.Error(), "only valid on the Bedrock Mantle access surface") {
		t.Fatalf("a Runtime provider kept a Bedrock project id: %v", err)
	}

	instance.AccessSurface = SurfaceBedrockMantle
	instance.ProfileID = ProfileBedrockMantleAnthropicMessages
	instance.CredentialScheme = CredentialBedrockAPIKey
	instance.BaseURL = "https://bedrock-mantle.us-east-1.api.aws"
	instance.AllowedHosts = []string{"bedrock-mantle.us-east-1.api.aws"}
	instance.Capabilities = DefaultProviderCapabilitiesForProfile(ProviderBedrock, ProfileBedrockMantleAnthropicMessages)
	instance.CapabilityEvidence = EvidenceForCapabilities(instance.Capabilities, EvidenceDeclared)
	if err := instance.Validate(); err != nil {
		t.Fatalf("a Mantle provider with a project id was rejected: %v", err)
	}
}
