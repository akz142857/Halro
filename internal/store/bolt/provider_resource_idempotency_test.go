package bolt

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
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

func TestProviderResourceWaitsForConfirmationBeforeInFlightReturns(t *testing.T) {
	store, instance, deployment, project := tombstoneChain(t)
	var nextIndex uint64
	var waits []uint64
	if err := store.SetMetadataJournalAfterDurable(func(metadatajournal.DurableBatch) (uint64, error) {
		nextIndex++
		return nextIndex, nil
	}, func(_ context.Context, index uint64) error {
		waits = append(waits, index)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record, err := store.PutProviderResource(context.Background(), keyedResource(
		project.ID, instance.ID, deployment.ID, instance.ProfileID, "idm_confirmed", sha256.Sum256([]byte("confirmed-before-provider")), now,
	), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(waits) != 0 {
		t.Fatalf("reserved resource unexpectedly waited: %v", waits)
	}
	record.CreationStatus = "in_flight"
	record.UpdatedAt = now.Add(time.Second)
	if _, err := store.PutProviderResource(context.Background(), record, record.Revision); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 || waits[0] != 2 {
		t.Fatalf("in-flight transition waits=%v, want [2]", waits)
	}
}

func TestBatchedInferenceClaimsIsolateKeyConflicts(t *testing.T) {
	store, instance, deployment, project := tombstoneChain(t)
	store.batches.size = 3
	store.batches.delay = time.Second
	before := store.MetadataWriteStats()
	now := time.Now().UTC()
	sharedHash := sha256.Sum256([]byte("shared-key"))
	otherHash := sha256.Sum256([]byte("other-key"))
	resources := []domain.ProviderResource{
		keyedResource(project.ID, instance.ID, deployment.ID, instance.ProfileID, "idm_first", sharedHash, now),
		keyedResource(project.ID, instance.ID, deployment.ID, instance.ProfileID, "idm_racing", sharedHash, now),
		keyedResource(project.ID, instance.ID, deployment.ID, instance.ProfileID, "idm_unrelated", otherHash, now),
	}
	type result struct {
		resource domain.ProviderResource
		err      error
	}
	results := make(chan result, len(resources))
	for _, claim := range resources {
		go func() {
			stored, err := store.PutProviderResource(context.Background(), claim, 0)
			results <- result{resource: stored, err: err}
		}()
	}
	var successes, conflicts int
	for range resources {
		got := <-results
		switch {
		case errors.Is(got.err, ErrAlreadyExists) && got.resource.IdempotencyKeyHash == sharedHash:
			conflicts++
		case got.err == nil && got.resource.Revision == 1:
			successes++
			indexed, found, err := store.ProviderResourceByIdempotency(context.Background(), project.ID,
				domain.ResourceInferenceCall, got.resource.IdempotencyKeyHash)
			if err != nil || !found || indexed.ID != got.resource.ID || indexed.Revision != 1 {
				t.Errorf("claim %s: indexed=%+v found=%v err=%v", got.resource.ID, indexed, found, err)
			}
		default:
			t.Errorf("claim %s: revision=%d err=%v", got.resource.ID, got.resource.Revision, got.err)
		}
	}
	if successes != 2 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d, want 2 and 1", successes, conflicts)
	}
	after := store.MetadataWriteStats()
	if calls, transactions := after.BatchCalls-before.BatchCalls, after.BatchTransactions-before.BatchTransactions; calls != 3 || transactions != 1 {
		t.Fatalf("batch calls=%d transactions=%d, want 3 and 1", calls, transactions)
	}
	// The lost race is an expected outcome, so it must not roll the shared
	// transaction back and re-run its siblings (ADR 0012's first precondition).
	if retries := after.BatchRetries - before.BatchRetries; retries != 0 {
		t.Fatalf("a same-key conflict rolled the batch back %d times", retries)
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
	if err := store.view(func(tx *Tx) error {
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
	if err := store.view(func(tx *Tx) error {
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
	if err := store.view(func(tx *Tx) error {
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
	if err := store.update(func(tx *Tx) error {
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

	migrated, err := openForTest(t, path)
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

// Outside a batch a lost race rolls back: committing the empty transaction
// instead would still write and sync a meta page for every losing duplicate.
func TestUnbatchedKeyConflictWritesNothing(t *testing.T) {
	store, instance, deployment, project := tombstoneChain(t)
	if err := store.SetMetadataJournalAfterDurable(func(metadatajournal.DurableBatch) (uint64, error) {
		return 1, nil
	}, func(context.Context, uint64) error { return nil }); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	keyHash := sha256.Sum256([]byte("replicated-key"))
	if _, err := store.PutProviderResource(context.Background(),
		keyedResource(project.ID, instance.ID, deployment.ID, instance.ProfileID, "idm_winner", keyHash, now), 0); err != nil {
		t.Fatal(err)
	}
	before := store.MetadataWriteStats()
	_, err := store.PutProviderResource(context.Background(),
		keyedResource(project.ID, instance.ID, deployment.ID, instance.ProfileID, "idm_loser", keyHash, now), 0)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("losing duplicate = %v, want ErrAlreadyExists", err)
	}
	if writes := store.MetadataWriteStats().PageWrites - before.PageWrites; writes != 0 {
		t.Fatalf("a lost race wrote %d pages", writes)
	}
}
