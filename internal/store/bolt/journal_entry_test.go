package bolt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
	bbolt "go.etcd.io/bbolt"
)

func journalHead(t *testing.T, store *Store) metadatajournal.Head {
	t.Helper()
	head, err := metadatajournal.Verify(store.MetadataJournalPath(), testJournalKey())
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func appliedSequence(t *testing.T, store *Store) uint64 {
	t.Helper()
	state, err := store.MetadataJournalState()
	if err != nil {
		t.Fatal(err)
	}
	return state.Applied
}

// TestAnAuthoritativeWriteRecordsAFrameBeforeItCommits is the ordering the
// whole design rests on. The projection is level with the journal or behind it,
// never ahead: a crash between the fsync and the commit is recoverable, and the
// reverse — a committed write no log describes — is not.
func TestAnAuthoritativeWriteRecordsAFrameBeforeItCommits(t *testing.T) {
	store := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	before := journalHead(t, store)
	if _, err := store.PutProject(context.Background(), domain.Project{
		ID: "prj_1", Name: "one", Enabled: true,
	}, 0, nil); err != nil {
		t.Fatal(err)
	}
	after := journalHead(t, store)
	if after.Sequence != before.Sequence+1 {
		t.Fatalf("a project write moved the journal from %d to %d", before.Sequence, after.Sequence)
	}
	if applied := appliedSequence(t, store); applied != after.Sequence {
		t.Fatalf("the projection is at %d and the journal at %d", applied, after.Sequence)
	}
}

// TestNodeDerivedWritesRecordNothing. A Replica advances its own checkpoints
// from its own Ledger, so shipping the Primary's would state one node's
// observations as another's — and it would put the request path's per-minute
// checkpoint writes into a log that is supposed to carry authoritative changes.
func TestNodeDerivedWritesRecordNothing(t *testing.T) {
	store := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	before := journalHead(t, store)
	if err := store.PutRouteSuspension(context.Background(), domain.RouteSuspension{
		ScopeKind: "deployment", ScopeKey: "dep_1", Reason: "invalid_credential",
		ObservedAt: time.Now().UTC(), Indefinite: true,
	}); err != nil {
		t.Fatal(err)
	}
	if after := journalHead(t, store); after.Sequence != before.Sequence {
		t.Fatalf("a route suspension recorded a frame: %d -> %d", before.Sequence, after.Sequence)
	}
	// And it still committed.
	rows, err := store.ListRouteSuspensions(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
}

// TestAnUnclassifiedBucketIsRefusedRatherThanRecordedOrDropped. Guessing in the
// node-local direction drops an authoritative write from the log and nothing
// says so until a failover; guessing the other way ships bytes that should stay
// local. There is no third answer, so there is no default.
func TestAnUnclassifiedBucketIsRefusedRatherThanRecordedOrDropped(t *testing.T) {
	store := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	err := store.update(func(tx *Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte("a_bucket_nobody_classified"))
		if err != nil {
			return err
		}
		return bucket.Put([]byte("k"), []byte("v"))
	})
	if err == nil || !strings.Contains(err.Error(), "no journal class") {
		t.Fatalf("an unclassified bucket was accepted: %v", err)
	}
	// And the transaction rolled back rather than leaving the bucket behind.
	if err := store.view(func(tx *Tx) error {
		if tx.Bucket([]byte("a_bucket_nobody_classified")) != nil {
			return errors.New("the refused transaction left its bucket behind")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestACrashBetweenTheFrameAndTheCommitIsReplayed.
//
// Simulated by rewinding the projection's recorded position, which is exactly
// the state such a crash leaves: the frame is on disk and the transaction that
// would have advanced the position never committed.
func TestACrashBetweenTheFrameAndTheCommitIsReplayed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store := openJournalledStore(t, path)
	for _, id := range []string{"prj_1", "prj_2", "prj_3"} {
		if _, err := store.PutProject(context.Background(), domain.Project{
			ID: id, Name: id, Enabled: true,
		}, 0, nil); err != nil {
			t.Fatal(err)
		}
	}
	head := journalHead(t, store)
	// Rewind the position by two transactions and remove what they wrote, so
	// the replay has something observable to restore.
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if err := meta.Put(keyAppliedJournalSequence, encodeUint64(head.Sequence-2)); err != nil {
			return err
		}
		projects := tx.Bucket(bucketProjects)
		if err := projects.Delete([]byte("prj_2")); err != nil {
			return err
		}
		return projects.Delete([]byte("prj_3"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	state, err := reopened.AttachMetadataJournal(testJournalKey(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if state.Replayed != 2 {
		t.Fatalf("replayed %d transactions, want 2", state.Replayed)
	}
	if state.Applied != head.Sequence {
		t.Fatalf("the projection is at %d and the journal at %d", state.Applied, head.Sequence)
	}
	for _, id := range []string{"prj_1", "prj_2", "prj_3"} {
		if _, err := reopened.GetProject(context.Background(), id); err != nil {
			t.Fatalf("project %s did not come back from the journal: %v", id, err)
		}
	}
}

// TestReplayIsIdempotent. An interrupted replay simply happens again on the
// next start, which is only safe because every recorded operation is idempotent
// by construction.
func TestReplayIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store := openJournalledStore(t, path)
	if _, err := store.PutProject(context.Background(), domain.Project{
		ID: "prj_1", Name: "one", Enabled: true,
	}, 0, nil); err != nil {
		t.Fatal(err)
	}
	head := journalHead(t, store)
	for range 3 {
		if err := store.db.Update(func(tx *bbolt.Tx) error {
			return tx.Bucket(bucketMeta).Put(keyAppliedJournalSequence, encodeUint64(0))
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store = openJournalledStore(t, path)
		if applied := appliedSequence(t, store); applied != head.Sequence {
			t.Fatalf("after a repeated replay the projection is at %d, want %d", applied, head.Sequence)
		}
		project, err := store.GetProject(context.Background(), "prj_1")
		if err != nil || project.Name != "one" {
			t.Fatalf("repeated replay changed the record: %#v %v", project, err)
		}
	}
}

// TestAProjectionAheadOfItsJournalFailsClosed.
//
// No ordering this code performs can produce it, so it means a directory that
// was edited, restored by hand, or written by another binary. Trusting the
// projection would discard recorded writes; replaying over it could overwrite
// newer state. Refusing to open is the only answer that loses nothing.
func TestAProjectionAheadOfItsJournalFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store := openJournalledStore(t, path)
	if _, err := store.PutProject(context.Background(), domain.Project{
		ID: "prj_1", Name: "one", Enabled: true,
	}, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(keyAppliedJournalSequence, encodeUint64(9999))
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	_, err = reopened.AttachMetadataJournal(testJournalKey(), "test")
	if !errors.Is(err, ErrJournalDiverged) {
		t.Fatalf("a projection ahead of its journal opened: %v", err)
	}
}

// TestAnEpochMismatchFailsClosed. A database following one epoch and a file
// holding another describe different projections, and nothing in this process
// can decide which is right.
func TestAnEpochMismatchFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store := openJournalledStore(t, path)
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(keyMetadataJournalEpoch, encodeUint64(7))
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.AttachMetadataJournal(testJournalKey(), "test"); !errors.Is(err, ErrJournalDiverged) {
		t.Fatalf("an epoch mismatch opened: %v", err)
	}
}

// TestAMissingJournalOnADatabaseThatFollowedOneFailsClosed. The file being gone
// is not the same as never having had one: the projection records a position in
// a chain, and a silently fresh epoch would discard whatever that chain still
// described.
func TestAMissingJournalOnADatabaseThatFollowedOneFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store := openJournalledStore(t, path)
	journalPath := store.MetadataJournalPath()
	if _, err := store.PutProject(context.Background(), domain.Project{
		ID: "prj_1", Name: "one", Enabled: true,
	}, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(journalPath); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state, err := reopened.AttachMetadataJournal(testJournalKey(), "test")
	if err != nil {
		t.Fatal(err)
	}
	// A new epoch, not a continuation: the previous one's frames are gone and
	// this database is the new epoch's starting state.
	if !state.StartedEpoch || state.Epoch != 2 {
		t.Fatalf("a missing journal did not open a new epoch: %+v", state)
	}
}

// TestBatchedWritesShareOneFrame is what the coalescing layer buys: the frame
// and its fsync are per transaction, not per caller.
func TestBatchedWritesShareOneFrame(t *testing.T) {
	store := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	before := journalHead(t, store)
	var group sync.WaitGroup
	errs := make([]error, 16)
	for index := range errs {
		group.Add(1)
		go func() {
			defer group.Done()
			errs[index] = store.batch(func(tx *Tx) error {
				return tx.Bucket(bucketRedactionPolicies).Put(
					[]byte{byte(index)}, []byte("policy"))
			})
		}()
	}
	group.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("batched write %d failed: %v", index, err)
		}
	}
	after := journalHead(t, store)
	if after.Sequence-before.Sequence >= uint64(len(errs)) {
		t.Fatalf("16 batched writes produced %d frames; nothing coalesced", after.Sequence-before.Sequence)
	}
	if applied := appliedSequence(t, store); applied != after.Sequence {
		t.Fatalf("the projection is at %d and the journal at %d", applied, after.Sequence)
	}
}

// TestAFailingBatchSiblingDoesNotReachTheJournal.
//
// The property db.Batch had and this layer has to keep: one caller's error
// cannot fail another's write. What is new is the second half — the failed
// attempt's operations must never be recorded, because the transaction that
// would have committed them was rolled back.
func TestAFailingBatchSiblingDoesNotReachTheJournal(t *testing.T) {
	store := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	poison := errors.New("poisoned batch sibling")
	var group sync.WaitGroup
	results := make([]error, 4)
	group.Add(len(results))
	for index := range results {
		go func() {
			defer group.Done()
			results[index] = store.batch(func(tx *Tx) error {
				if index == 1 {
					// Write first, then fail: the rollback has to undo this.
					if err := tx.Bucket(bucketRedactionPolicies).Put([]byte("poisoned"), []byte("x")); err != nil {
						return err
					}
					return poison
				}
				return tx.Bucket(bucketRedactionPolicies).Put([]byte{byte(index)}, []byte("policy"))
			})
		}()
	}
	group.Wait()
	for index, err := range results {
		if index == 1 {
			if !errors.Is(err, poison) {
				t.Fatalf("the poisoned caller got %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("sibling %d was failed by the poisoned caller: %v", index, err)
		}
	}
	if err := store.view(func(tx *Tx) error {
		if tx.Bucket(bucketRedactionPolicies).Get([]byte("poisoned")) != nil {
			return errors.New("the failed callback's write survived")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := metadatajournal.Replay(store.MetadataJournalPath(), testJournalKey(),
		func(record metadatajournal.Record) error {
			for _, op := range record.Ops {
				if string(op.Key) == "poisoned" {
					return errors.New("the failed callback's operation reached the journal")
				}
			}
			return nil
		}); err != nil {
		t.Fatal(err)
	}
}

// TestTrimKeepsTheProjectionReadable. The journal is on the request path — two
// price-pin frames per Attempt with a Deployment — so a file that never shrinks
// grows with traffic rather than with administration.
func TestTrimKeepsTheProjectionReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store := openJournalledStore(t, path)
	for index := range 12 {
		if _, err := store.PutProject(context.Background(), domain.Project{
			ID: "prj_" + string(rune('a'+index)), Name: "p", Enabled: true,
		}, 0, nil); err != nil {
			t.Fatal(err)
		}
	}
	applied := appliedSequence(t, store)
	state, err := store.TrimMetadataJournal(applied - 4)
	if err != nil {
		t.Fatal(err)
	}
	if state.TrimmedThrough != applied-4 {
		t.Fatalf("trim reported %+v", state)
	}
	// The projection is intact and still writable afterwards.
	if _, err := store.PutProject(context.Background(), domain.Project{
		ID: "prj_after", Name: "after", Enabled: true,
	}, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openJournalledStore(t, path)
	if _, err := reopened.GetProject(context.Background(), "prj_after"); err != nil {
		t.Fatal(err)
	}
}

// TestTrimRefusesToOutrunTheProjection. Dropping frames the projection has not
// applied would leave a gap nothing can replay.
func TestTrimRefusesToOutrunTheProjection(t *testing.T) {
	store := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	if _, err := store.PutProject(context.Background(), domain.Project{
		ID: "prj_1", Name: "one", Enabled: true,
	}, 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TrimMetadataJournal(appliedSequence(t, store) + 1); err == nil {
		t.Fatal("a trim past the projection was accepted")
	}
}

// TestAWholeFilePublishRecordsNothing. A staged database is how the next
// projection is built, not an operation inside one — the rename is what starts
// the next epoch.
func TestAWholeFilePublishRecordsNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store, err := OpenForWholeFilePublish(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.PutProject(context.Background(), domain.Project{
		ID: "prj_1", Name: "one", Enabled: true,
	}, 0, nil); err != nil {
		t.Fatalf("a staged publish was refused: %v", err)
	}
	if _, err := os.Stat(store.MetadataJournalPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a staged publish created a journal: %v", err)
	}
}

// TestTheJournalIsRefusedWithoutAnAttach is the fail-closed gate itself: a
// write that exists in the projection and in no log is precisely the state the
// journal exists to make impossible.
func TestTheJournalIsRefusedWithoutAnAttach(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.PutProject(context.Background(), domain.Project{
		ID: "prj_1", Name: "one", Enabled: true,
	}, 0, nil)
	if !errors.Is(err, ErrJournalUnavailable) {
		t.Fatalf("an unrecorded authoritative write was accepted: %v", err)
	}
	// Node-derived writes still work: they record nothing by construction, so
	// there is nothing for a missing journal to fail to record.
	if err := store.PutRouteSuspension(context.Background(), domain.RouteSuspension{
		ScopeKind: "deployment", ScopeKey: "dep_1", Reason: "invalid_credential",
		ObservedAt: time.Now().UTC(), Indefinite: true,
	}); err != nil {
		t.Fatalf("a node-derived write needed a journal: %v", err)
	}
}
