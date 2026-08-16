package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The console's tests need to know what this endpoint answers. Writing that by
// hand would test the fixture author's idea of the matrix rather than the
// matrix, and a console built against a wrong idea of it is the drift this whole
// change exists to remove.
//
// So the fixture is generated from the real response and checked in. This test
// fails when the two diverge; HALRO_UPDATE_GOLDEN=1 rewrites it. Same shape as
// the endpoint-manifests golden in internal/compatibility.
func TestProviderProfilesGoldenMatchesConsoleFixture(t *testing.T) {
	runtime, cookie := providerProfilesFixture(t)
	body := fetchProviderProfilesBody(t, runtime, cookie)

	var indented map[string]any
	if err := json.Unmarshal([]byte(body), &indented); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(indented, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')

	goldenPath := filepath.Join("..", "..", "web", "src", "test", "provider-profiles.golden.json")
	if os.Getenv("HALRO_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("golden updated")
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v (run with HALRO_UPDATE_GOLDEN=1 to create it)", err)
	}
	if string(want) != string(encoded) {
		t.Fatalf("the console fixture no longer matches what the endpoint answers.\n"+
			"Re-run with HALRO_UPDATE_GOLDEN=1 and review the diff — a change here changes\n"+
			"what every connection form offers.\ngot:\n%s", encoded)
	}
}
