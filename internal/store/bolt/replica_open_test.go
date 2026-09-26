package bolt

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	bbolt "go.etcd.io/bbolt"
)

func setSchemaVersionForReplicaTest(t *testing.T, path string, version uint64) {
	t.Helper()
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, encodeUint64(version))
	}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenReplicaDoesNotMigrateOrRejectABehindSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "halro.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	behind := schemaVersion - 1
	setSchemaVersionForReplicaTest(t, path, behind)
	replica, err := OpenReplica(path)
	if err != nil {
		t.Fatal(err)
	}
	version, err := replica.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if version != behind {
		t.Fatalf("replica schema=%d, want unchanged %d", version, behind)
	}
	if err := replica.Close(); err != nil {
		t.Fatal(err)
	}
	if readOnly, err := OpenReadOnly(path); !errors.Is(err, ErrSchemaVersionMismatch) {
		if readOnly != nil {
			readOnly.Close()
		}
		t.Fatalf("read-only exact gate error=%v", err)
	}
}

func TestOpenReplicaRefusesFutureSchemaAndMissingFile(t *testing.T) {
	directory := t.TempDir()
	missing := filepath.Join(directory, "missing.db")
	if store, err := OpenReplica(missing); !errors.Is(err, os.ErrNotExist) {
		if store != nil {
			store.Close()
		}
		t.Fatalf("missing error=%v", err)
	}
	path := filepath.Join(directory, "future.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	setSchemaVersionForReplicaTest(t, path, schemaVersion+1)
	if store, err := OpenReplica(path); !errors.Is(err, ErrSchemaVersionMismatch) {
		if store != nil {
			store.Close()
		}
		t.Fatalf("future schema error=%v", err)
	}
}
