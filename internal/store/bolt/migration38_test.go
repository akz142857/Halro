package bolt

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func TestMigration38FencesReadersWithoutRewritingProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	provider := domain.ProviderInstance{ID: "prov_egress_fence"}
	raw, err := json.Marshal(provider)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		if err := tx.Bucket(bucketProviders).Put([]byte(provider.ID), raw); err != nil {
			return err
		}
		if tx.Bucket(bucketProviderEgressProxies) != nil {
			if err := tx.DeleteBucket(bucketProviderEgressProxies); err != nil {
				return err
			}
		}
		if history := tx.Bucket(bucketMigrationHistory); history != nil {
			if err := history.Delete(versionKey(38)); err != nil {
				return err
			}
		}
		var version [8]byte
		binary.BigEndian.PutUint64(version[:], 37)
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, version[:])
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrated.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket(bucketProviderEgressProxies) == nil {
			t.Fatal("migration 38 did not create the managed proxy bucket")
		}
		stored := tx.Bucket(bucketProviders).Get([]byte(provider.ID))
		if string(stored) != string(raw) {
			t.Fatalf("no-op fence rewrote Provider JSON: got %s want %s", stored, raw)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}

	// A schema-37 reader's exact gate: the data directory is now newer, so it
	// must refuse before any runtime or listener is built.
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.View(func(tx *bbolt.Tx) error {
		version := binary.BigEndian.Uint64(tx.Bucket(bucketMeta).Get(keySchemaVersion))
		if version == 37 {
			return nil
		}
		return ErrSchemaVersionMismatch
	}); !errors.Is(err, ErrSchemaVersionMismatch) {
		t.Fatalf("schema-37 reader did not refuse schema 38: %v", err)
	}
}
