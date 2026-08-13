package compatibility

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBuiltinEndpointManifestsAreValidImmutableAndGolden(t *testing.T) {
	manifests := BuiltinEndpointManifests()
	for _, manifest := range manifests {
		if err := manifest.Validate(); err != nil {
			t.Fatalf("%s: %v", manifest.ID, err)
		}
	}
	manifests[0].RequestFields[0] = "mutated"
	if BuiltinEndpointManifests()[0].RequestFields[0] == "mutated" {
		t.Fatal("builtin manifest shared mutable data")
	}
	encoded, err := json.MarshalIndent(BuiltinEndpointManifests(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	goldenPath := filepath.Join("..", "..", "docs", "compatibility", "endpoint-manifests.json")
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("HALRO_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		want = encoded
	}
	if string(encoded) != string(want) {
		t.Fatalf("endpoint manifest snapshot changed; review compatibility claims and update the golden file deliberately\n--- got ---\n%s", encoded)
	}
}

func TestInferenceResourcesMaturityDoesNotClaimUnvalidatedSDKCompatibility(t *testing.T) {
	for _, manifest := range inferenceResourcesEndpointManifests() {
		if manifest.Status != StatusExperimental {
			t.Fatalf("%s status = %q, want experimental until its official SDK matrix passes", manifest.ID, manifest.Status)
		}
		if len(manifest.SDKMatrix) != 0 {
			t.Fatalf("%s claims an unvalidated SDK matrix: %v", manifest.ID, manifest.SDKMatrix)
		}
		if strings.HasPrefix(manifest.ID, "halro.") && manifest.Protocol != "halro" {
			t.Fatalf("%s protocol = %q, want halro", manifest.ID, manifest.Protocol)
		}
		if strings.HasPrefix(manifest.ID, "halro.") && manifest.NorthboundProfile != ProfileHalroInferenceResources {
			t.Fatalf("%s northbound profile = %q, want %q", manifest.ID, manifest.NorthboundProfile, ProfileHalroInferenceResources)
		}
	}
}

func TestEmbeddingsProfileMaturityDoesNotPromoteTitanEmbed(t *testing.T) {
	var embeddings EndpointCompatibilityManifest
	for _, manifest := range BuiltinEndpointManifests() {
		if manifest.ID == "openai.embeddings.v1" {
			embeddings = manifest
			break
		}
	}
	if embeddings.Status != StatusCompatible {
		t.Fatalf("embeddings endpoint status = %q, want compatible", embeddings.Status)
	}
	for _, coverage := range embeddings.ProfileCoverage {
		want := StatusCompatible
		if coverage.ProfileID == "bedrock.runtime.invoke.titan-embed-text-v2.v1" {
			want = StatusExperimental
		}
		if coverage.Status != want {
			t.Fatalf("%s status = %q, want %q", coverage.ProfileID, coverage.Status, want)
		}
	}
}

func TestManifestRejectsMissingProfileMaturity(t *testing.T) {
	manifest := CloneEndpointManifest(BuiltinEndpointManifests()[0])
	manifest.ProfileCoverage[0].Status = ""
	if err := manifest.Validate(); err == nil {
		t.Fatal("profile coverage without a maturity status was accepted")
	}
}

func TestManifestRejectsInferenceResourcesProfilePromotion(t *testing.T) {
	var manifest EndpointCompatibilityManifest
	for _, candidate := range BuiltinEndpointManifests() {
		if candidate.ID == "openai.embeddings.v1" {
			manifest = CloneEndpointManifest(candidate)
			break
		}
	}
	for index := range manifest.ProfileCoverage {
		if manifest.ProfileCoverage[index].ProfileID == "bedrock.runtime.invoke.titan-embed-text-v2.v1" {
			manifest.ProfileCoverage[index].Status = StatusCompatible
		}
	}
	if err := manifest.Validate(); err == nil {
		t.Fatal("phase 2 provider profile was promoted through a compatible endpoint")
	}
}

func TestManifestRejectsNorthboundProfileDrift(t *testing.T) {
	base := CloneEndpointManifest(BuiltinEndpointManifests()[0])
	tests := []struct {
		name   string
		mutate func(*EndpointCompatibilityManifest)
	}{
		{name: "unknown profile", mutate: func(manifest *EndpointCompatibilityManifest) { manifest.NorthboundProfile = "missing.v1" }},
		{name: "protocol mismatch", mutate: func(manifest *EndpointCompatibilityManifest) { manifest.Protocol = "halro" }},
		{name: "revision mismatch", mutate: func(manifest *EndpointCompatibilityManifest) { manifest.ProfileRevision++ }},
		{name: "method mismatch", mutate: func(manifest *EndpointCompatibilityManifest) { manifest.Method = "GET" }},
		{name: "path mismatch", mutate: func(manifest *EndpointCompatibilityManifest) { manifest.Path = "/v1/not-chat" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := CloneEndpointManifest(base)
			test.mutate(&manifest)
			if err := manifest.Validate(); err == nil {
				t.Fatal("invalid northbound profile binding was accepted")
			}
		})
	}
}

func TestInferenceResourcesManifestMatchesImplementedRequestAndResponseFields(t *testing.T) {
	byID := make(map[string]EndpointCompatibilityManifest)
	for _, manifest := range inferenceResourcesEndpointManifests() {
		byID[manifest.ID] = manifest
	}
	image := byID["openai.images.generations.v1"]
	if slices.Contains(image.RequestFields, "user") || !slices.Contains(image.RejectedRequestFields, "user") {
		t.Fatalf("image user field contract is inaccurate: accepted=%v rejected=%v", image.RequestFields, image.RejectedRequestFields)
	}
	for _, id := range []string{"openai.batches.create.v1", "openai.batches.get.v1", "openai.batches.cancel.v1"} {
		if !slices.Equal(byID[id].ResponseFields, batchResponseFields()) {
			t.Fatalf("%s response fields drifted: %v", id, byID[id].ResponseFields)
		}
	}
	for _, id := range []string{"halro.async.create.v1", "halro.async.get.v1"} {
		if !slices.Equal(byID[id].ResponseFields, asyncResponseFields()) {
			t.Fatalf("%s response fields drifted: %v", id, byID[id].ResponseFields)
		}
	}
}

// Evidence and the SDK matrix are two ways of saying the same thing, and the
// failure they invite is drift: an endpoint that gains an SDK suite without
// claiming the evidence, or claims it without naming the SDKs. The first
// understates what exists and the second overstates it, and overstating is the
// one a reader cannot detect.
func TestEndpointEvidenceCannotDriftFromTheSDKMatrix(t *testing.T) {
	for _, manifest := range BuiltinEndpointManifests() {
		claimed := slices.Contains(manifest.Evidence, EvidenceSDKBlackBox)
		if claimed != (len(manifest.SDKMatrix) > 0) {
			t.Errorf("%s: sdk_blackbox evidence=%v but sdk_matrix has %d entries",
				manifest.ID, claimed, len(manifest.SDKMatrix))
		}
		if manifest.Status == StatusCompatible && !claimed {
			t.Errorf("%s is compatible without SDK black-box evidence", manifest.ID)
		}
	}
}

// A status is a verdict and evidence is its basis, so every endpoint has to
// declare one. An endpoint that declares none is not "unverified" — it is
// unaccounted for, and there is no way to tell the two apart from the outside.
func TestEveryEndpointDeclaresItsEvidence(t *testing.T) {
	for _, manifest := range BuiltinEndpointManifests() {
		if len(manifest.Evidence) == 0 {
			t.Errorf("%s declares no evidence", manifest.ID)
		}
		for _, kind := range manifest.Evidence {
			if !kind.Valid() {
				t.Errorf("%s declares evidence kind %q, which is not a recognised kind", manifest.ID, kind)
			}
		}
	}
}
