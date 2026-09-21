package bolt

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func keyedResource(project, provider, deployment string, profile domain.ProviderProfileID, id string, keyHash [32]byte, now time.Time) domain.ProviderResource {
	return domain.ProviderResource{
		ID: id, Kind: domain.ResourceInferenceCall, ProjectID: project,
		ProviderID: provider, DeploymentID: deployment, PublicModel: "model", ProfileID: profile,
		IdempotencyKeyHash: keyHash, RequestFingerprint: sha256.Sum256([]byte("body")),
		CreationStatus: "reserved", Status: "pending",
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(24 * time.Hour),
	}
}

// Absence is an answer, not a failure. Folding "this key is free" into the same
// error value as "the store could not say" is what let a read failure be read
// as a free key, and the caller cannot tell them apart if the store will not.
func TestIdempotencyLookupReportsAbsenceWithoutAnError(t *testing.T) {
	store, instance, deployment, project := tombstoneChain(t)
	ctx := context.Background()
	now := time.Now().UTC()
	keyHash := sha256.Sum256([]byte("used-once"))

	if _, found, err := store.ProviderResourceByIdempotency(ctx, project.ID, domain.ResourceInferenceCall, keyHash); err != nil || found {
		t.Fatalf("unused key: found=%v err=%v, want found=false with no error", found, err)
	}
	created, err := store.PutProviderResource(ctx,
		keyedResource(project.ID, instance.ID, deployment.ID, instance.ProfileID, "idm_one", keyHash, now), 0)
	if err != nil {
		t.Fatal(err)
	}
	found, ok, err := store.ProviderResourceByIdempotency(ctx, project.ID, domain.ResourceInferenceCall, keyHash)
	if err != nil || !ok || found.ID != created.ID {
		t.Fatalf("used key: found=%+v ok=%v err=%v", found, ok, err)
	}
	// The same key in another Project, and the same key under another kind, are
	// different keys. The index entry carries both, so neither collides.
	if _, ok, err := store.ProviderResourceByIdempotency(ctx, "project_other", domain.ResourceInferenceCall, keyHash); err != nil || ok {
		t.Fatalf("cross-project: ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.ProviderResourceByIdempotency(ctx, project.ID, domain.ResourceFile, keyHash); err != nil || ok {
		t.Fatalf("cross-kind: ok=%v err=%v", ok, err)
	}
	// The mechanism, not just its result: the lookup is one Get because the
	// write maintains an entry, and a test that only asked the accessor would
	// pass just as well against the full-bucket scan this replaced.
	if err := store.db.View(func(tx *bbolt.Tx) error {
		entry := tx.Bucket(bucketProviderResourceIdem).Get(providerResourceIdemKey(project.ID, domain.ResourceInferenceCall, keyHash))
		if string(entry) != created.ID {
			t.Fatalf("index entry = %q, want %q", entry, created.ID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// The reaper deletes the record; the key has to become free with it, or the
// index outlives what it names and refuses a key nothing holds.
func TestDeletingAResourceFreesItsIdempotencyKey(t *testing.T) {
	store, instance, deployment, project := tombstoneChain(t)
	ctx := context.Background()
	now := time.Now().UTC()
	keyHash := sha256.Sum256([]byte("spent-then-reaped"))

	first := keyedResource(project.ID, instance.ID, deployment.ID, instance.ProfileID, "idm_first", keyHash, now)
	if _, err := store.PutProviderResource(ctx, first, 0); err != nil {
		t.Fatal(err)
	}
	second := keyedResource(project.ID, instance.ID, deployment.ID, instance.ProfileID, "idm_second", keyHash, now)
	if _, err := store.PutProviderResource(ctx, second, 0); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("err = %v, want the key to be held", err)
	}
	if err := store.DeleteProviderResource(ctx, project.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.ProviderResourceByIdempotency(ctx, project.ID, domain.ResourceInferenceCall, keyHash); err != nil || ok {
		t.Fatalf("the deleted record still holds its key: ok=%v err=%v", ok, err)
	}
	// The entry goes with the record, rather than being tolerated as an orphan
	// the next lookup steps over. The reader does tolerate one, so nothing the
	// caller sees would notice — but every reaped record would leave an entry
	// behind, and the bucket the index exists to stop walking would be the one
	// growing without bound.
	if err := store.db.View(func(tx *bbolt.Tx) error {
		if entry := tx.Bucket(bucketProviderResourceIdem).Get(providerResourceIdemKey(project.ID, domain.ResourceInferenceCall, keyHash)); entry != nil {
			t.Fatalf("the deleted record left an index entry naming %q", entry)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutProviderResource(ctx, second, 0); err != nil {
		t.Fatalf("the freed key was refused: %v", err)
	}
	if err := store.db.View(func(tx *bbolt.Tx) error {
		entry := tx.Bucket(bucketProviderResourceIdem).Get(providerResourceIdemKey(project.ID, domain.ResourceInferenceCall, keyHash))
		if string(entry) != second.ID {
			t.Fatalf("index entry = %q, want it to name the record that now holds the key", entry)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// An index built lazily would read as "this key is free" for every record an
// upgrade inherited, which is exactly the duplicate upstream call the key is
// sent to prevent. So schema 40 builds it from what is already on disk.
func TestSchemaFortyBuildsTheIndexFromExistingRecords(t *testing.T) {
	store, instance, deployment, project := tombstoneChain(t)
	ctx := context.Background()
	now := time.Now().UTC()
	keyHash := sha256.Sum256([]byte("written-before-the-index"))
	if _, err := store.PutProviderResource(ctx,
		keyedResource(project.ID, instance.ID, deployment.ID, instance.ProfileID, "idm_inherited", keyHash, now), 0); err != nil {
		t.Fatal(err)
	}
	path := store.db.Path()

	// Back to schema 39: no index bucket, no migration record, and a record
	// that has to be found without either.
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		if err := tx.DeleteBucket(bucketProviderResourceIdem); err != nil {
			return err
		}
		if err := tx.Bucket(bucketMigrationHistory).Delete(versionKey(40)); err != nil {
			return err
		}
		var version [8]byte
		binary.BigEndian.PutUint64(version[:], 39)
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
	defer migrated.Close()
	if version, err := migrated.SchemaVersion(); err != nil || version != schemaVersion {
		t.Fatalf("schema = %d err = %v, want %d", version, err, schemaVersion)
	}
	found, ok, err := migrated.ProviderResourceByIdempotency(ctx, project.ID, domain.ResourceInferenceCall, keyHash)
	if err != nil || !ok || found.ID != "idm_inherited" {
		t.Fatalf("an inherited key was not indexed: found=%+v ok=%v err=%v", found, ok, err)
	}
}
