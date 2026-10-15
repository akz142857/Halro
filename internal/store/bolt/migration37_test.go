package bolt

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	bbolt "go.etcd.io/bbolt"
)

// Schema 37 is a compatibility fence for Usage attribution and canonical
// failure semantics. A pre-v0.8 binary only understands schema 36, so once the
// candidate has opened a data directory it must observe a newer metadata
// schema and refuse before binding listeners. The migration also discards the
// derivative checkpoint so this build replays the authenticated Ledger rather
// than trusting a v13 reader that silently ignored the new fields.
func TestMigration37FencesOldReadersAndDropsTheUsageCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(keyUsageCheckpoint, []byte(`{"watermark":{"generation":1,"sequence":1,"offset":1},"payload":"e30="}`))
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		if history := tx.Bucket(bucketMigrationHistory); history != nil {
			if err := history.Delete(versionKey(37)); err != nil {
				return err
			}
		}
		var version [8]byte
		binary.BigEndian.PutUint64(version[:], 36)
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, version[:])
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, killPoint := range []string{"before_usage_provider_attribution_boundary", "after_usage_provider_attribution_boundary"} {
		t.Run("atomic_"+killPoint, func(t *testing.T) {
			copyPath := filepath.Join(t.TempDir(), "metadata.db")
			payload, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(copyPath, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			migrated, err := openWithMigrationStepHook(copyPath, func(version uint64, point string) error {
				if version == 37 && point == killPoint {
					return errors.New("stop")
				}
				return nil
			})
			if migrated != nil {
				migrated.Close()
			}
			if err == nil {
				t.Fatal("interrupted migration succeeded")
			}
			readOnly, err := bbolt.Open(copyPath, 0o600, &bbolt.Options{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer readOnly.Close()
			if err := readOnly.View(func(tx *bbolt.Tx) error {
				if version := binary.BigEndian.Uint64(tx.Bucket(bucketMeta).Get(keySchemaVersion)); version != 36 {
					t.Fatalf("schema=%d, want rollback to 36", version)
				}
				if tx.Bucket(bucketMeta).Get(keyUsageCheckpoint) == nil {
					t.Fatal("interrupted migration did not roll back checkpoint deletion")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	if version, err := migrated.SchemaVersion(); err != nil || version != schemaVersion {
		t.Fatalf("schema=%d err=%v, want %d", version, err, schemaVersion)
	}
	if err := migrated.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket(bucketMeta).Get(keyUsageCheckpoint) != nil {
			t.Fatal("schema-13 usage checkpoint survived migration")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := migrated.db.View(func(tx *bbolt.Tx) error {
		stored := binary.BigEndian.Uint64(tx.Bucket(bucketMeta).Get(keySchemaVersion))
		if stored <= 36 {
			t.Fatalf("stored schema=%d does not fence a v0.7.1 reader", stored)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
