package bolt

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"path/filepath"
	"testing"

	bbolt "go.etcd.io/bbolt"
)

// Migration 29 moves a stored project's authorization list from the key that
// lied about its contents (allowed_routes) to the one that does not
// (allowed_models). The value was always public model aliases.
func TestMigration29RenamesProjectAllowedModels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")

	// A current-format directory, downgraded to schema 28 with one project
	// record still holding the legacy key.
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = store.db.Update(func(tx *bbolt.Tx) error {
		record := map[string]any{
			"id": "project_legacy", "name": "Legacy", "enabled": true,
			"allowed_routes": []string{"chat", "embed"},
			"revision":       1,
			"created_at":     "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z",
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketProjects).Put([]byte("project_legacy"), encoded); err != nil {
			return err
		}
		var version [8]byte
		binary.BigEndian.PutUint64(version[:], 28)
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, version[:])
	})
	if closeErr := store.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	project, err := migrated.GetProject(context.Background(), "project_legacy")
	if err != nil {
		t.Fatal(err)
	}
	if len(project.AllowedModels) != 2 || project.AllowedModels[0] != "chat" || project.AllowedModels[1] != "embed" {
		t.Fatalf("allowed models were not carried across the rename: %#v", project.AllowedModels)
	}
	// The legacy key is gone from the stored bytes, not merely shadowed.
	err = migrated.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketProjects).Get([]byte("project_legacy"))
		var record map[string]json.RawMessage
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		if _, stale := record["allowed_routes"]; stale {
			t.Fatal("stored record still carries allowed_routes beside its replacement")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
