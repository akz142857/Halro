package compatibility

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	if os.Getenv("HEIMDALL_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		want = encoded
	}
	if string(encoded) != string(want) {
		t.Fatalf("endpoint manifest snapshot changed; review compatibility claims and update the golden file deliberately\n--- got ---\n%s", encoded)
	}
}
