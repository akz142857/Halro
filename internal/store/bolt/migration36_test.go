package bolt

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func TestMigration36BackfillsInferenceOnlyAndCreatesRunGovernanceBuckets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	project := domain.Project{ID: "prj_legacy", Name: "legacy", Enabled: true, AllowedModels: []string{}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if _, err := store.PutProject(context.Background(), project, 0, nil); err != nil {
		t.Fatal(err)
	}
	key := domain.GatewayKey{ID: "key_legacy", ProjectID: project.ID, Name: "legacy", HashVersion: 1, Enabled: true, CreatedAt: time.Now().UTC()}
	if _, err := store.PutGatewayKey(context.Background(), key, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketGatewayKeys).Get([]byte(key.ID))
		var legacy map[string]any
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return err
		}
		delete(legacy, "scopes")
		encoded, err := json.Marshal(legacy)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketGatewayKeys).Put([]byte(key.ID), encoded); err != nil {
			return err
		}
		if err := tx.Bucket(bucketMeta).Put(keyUsageCheckpoint, []byte("legacy-checkpoint")); err != nil {
			return err
		}
		if err := tx.Bucket(bucketUsageCheckpointSegments).Put(usageSegmentKey(1), []byte("legacy-segment")); err != nil {
			return err
		}
		if err := tx.Bucket(bucketMigrationHistory).Delete(versionKey(36)); err != nil {
			return err
		}
		var version [8]byte
		binary.BigEndian.PutUint64(version[:], 35)
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, version[:])
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	for _, killPoint := range []string{"before_run_governance_attribution", "after_run_governance_attribution"} {
		t.Run("atomic_"+killPoint, func(t *testing.T) {
			copyPath := filepath.Join(t.TempDir(), "metadata.db")
			bytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(copyPath, bytes, 0o600); err != nil {
				t.Fatal(err)
			}
			migrated, err := openWithMigrationStepHook(copyPath, func(version uint64, point string) error {
				if version == 36 && point == killPoint {
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
				version := binary.BigEndian.Uint64(tx.Bucket(bucketMeta).Get(keySchemaVersion))
				if version != 35 {
					t.Fatalf("schema=%d want rollback to 35", version)
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
	got, err := migrated.GetGatewayKey(context.Background(), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Scopes) != 1 || got.Scopes[0] != domain.GatewayScopeInference {
		t.Fatalf("migrated scopes=%v", got.Scopes)
	}
	gate, err := migrated.LedgerCompatibilityGate()
	if err != nil || gate.FeatureEpoch != 5 || gate.MinimumReaderVersion != "v5" {
		t.Fatalf("gate=%#v err=%v", gate, err)
	}
	if _, _, err := migrated.UsageCheckpoint(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale usage checkpoint survived attribution migration: %v", err)
	}
	if err := migrated.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketUsageCheckpointSegments)
		if bucket == nil {
			t.Fatal("usage checkpoint segment bucket is missing")
		}
		key, _ := bucket.Cursor().First()
		if key != nil {
			t.Fatalf("stale usage checkpoint segment survived: %q", key)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
