package bolt

import (
	"strings"
	"testing"
)

// The refusal has to fire on a directory that actually carries the retired tier
// — a fixture built from this package's own model would only test the model.
func TestMigration21RefusesADirectoryCarryingLegacyEvidence(t *testing.T) {
	path := t.TempDir() + "/metadata.db"
	createV3ProviderMetadata(t, path) // no evidence -> migration 6 stamps `legacy`

	store, err := Open(path)
	if err == nil {
		store.Close()
		t.Fatal("a directory carrying legacy capability evidence was opened")
	}
	for _, want := range []string{"legacy", "make reset CONFIRM=RESET", "re-initialise"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal omits %q: %v", want, err)
		}
	}
}

// The refusal must be narrow: a directory with no legacy evidence upgrades in
// place. A gate that refuses everything bricks every install.
func TestMigration21LeavesACleanDirectoryAlone(t *testing.T) {
	path := t.TempDir() + "/metadata.db"
	createV3ProviderMetadataWithEvidence(t, path, true)

	store, err := Open(path)
	if err != nil {
		t.Fatalf("a directory with no legacy evidence was refused: %v", err)
	}
	defer store.Close()
	version, err := store.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema=%d want %d", version, schemaVersion)
	}
}

// A freshly initialised directory is the ordinary case and must be untouched.
func TestMigration21DoesNotAffectAFreshDirectory(t *testing.T) {
	store, err := Open(t.TempDir() + "/metadata.db")
	if err != nil {
		t.Fatalf("a fresh directory was refused: %v", err)
	}
	defer store.Close()
	version, err := store.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema=%d want %d", version, schemaVersion)
	}
}
