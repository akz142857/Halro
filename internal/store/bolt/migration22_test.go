package bolt

import (
	"strings"
	"testing"
)

// The refusal must fire on a directory that actually holds the old shape, and
// it must be narrow enough that ordinary directories still open.
func TestMigration22RefusesRoutesWithNoDeployment(t *testing.T) {
	path := t.TempDir() + "/metadata.db"
	createV2MetadataWithRoutes(t, path, 3)

	store, err := Open(path)
	if err == nil {
		store.Close()
		t.Fatal("a directory holding deployment-less routes was opened")
	}
	for _, want := range []string{"3 route(s) without one", "make reset CONFIRM=RESET", "versioned price"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal omits %q: %v", want, err)
		}
	}
}

// A fresh directory is the ordinary case and must be untouched by the gate.
func TestMigration22DoesNotAffectAFreshDirectory(t *testing.T) {
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
