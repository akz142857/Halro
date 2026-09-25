package bolt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

// What a full disk does to a metadata write.
//
// The 2026-09-18 run recorded that real disk-full has no automated coverage
// (260918-PV-F-21). A real one still needs the target environment — a test
// cannot fill a filesystem it does not own — but the branch a full disk reaches
// can be exercised exactly, through the same injection seam the Ledger already
// has and that the HA design's release gate asks for on every durable store.
//
// The property under test is the one the journal exists for: the frame is
// fsynced *before* the bbolt transaction commits, so a frame that cannot be
// written must take the transaction down with it. The opposite — a committed
// write that no log describes — is the state the whole design is built to make
// impossible, and a full disk is the most ordinary way to reach it.

// faultDurability fails writes or syncs the way a full or failing disk does.
type faultDurability struct {
	file     *os.File
	writeErr error
	syncErr  error
	// after lets the first n writes through, so a failure can land partway
	// through a workload rather than only at its start.
	after  int
	writes int
}

func (d *faultDurability) Write(payload []byte) (int, error) {
	d.writes++
	if d.writeErr != nil && d.writes > d.after {
		return 0, d.writeErr
	}
	return d.file.Write(payload)
}

func (d *faultDurability) Sync() error {
	if d.syncErr != nil && d.writes > d.after {
		return d.syncErr
	}
	return d.file.Sync()
}

// injectJournalFault installs the seam for one test and removes it afterwards.
func injectJournalFault(t *testing.T, fault *faultDurability) {
	t.Helper()
	metadatajournal.WrapDurability = func(file *os.File) metadatajournal.DurabilityWriter {
		fault.file = file
		return fault
	}
	t.Cleanup(func() { metadatajournal.WrapDurability = nil })
}

// TestAFullDiskRefusesTheWriteRatherThanCommittingItUnrecorded.
func TestAFullDiskRefusesTheWriteRatherThanCommittingItUnrecorded(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		fault *faultDurability
	}{
		{"ENOSPC on the frame", &faultDurability{writeErr: syscall.ENOSPC}},
		{"EIO during the frame fsync", &faultDurability{syncErr: syscall.EIO}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "metadata.db")
			injectJournalFault(t, testCase.fault)
			store := openJournalledStore(t, path)

			_, err := store.PutProject(context.Background(), domain.Project{
				ID: "prj_full_disk", Name: "one", Enabled: true,
			}, 0, nil)
			if err == nil {
				t.Fatal("a metadata write committed while its frame could not be made durable")
			}
			// The transaction rolled back: the record is not in the projection
			// either, so there is nothing the journal is failing to describe.
			if _, getErr := store.GetProject(context.Background(), "prj_full_disk"); getErr == nil {
				t.Fatal("the refused write is present in the projection")
			}
		})
	}
}

// TestNodeDerivedWritesSurviveAFullJournalDisk.
//
// Derived state is not journalled, so a disk that cannot take a frame must not
// stop a checkpoint from advancing. It matters in exactly the situation this
// models: an instance whose disk is filling needs its usage checkpoint and its
// route suspensions to keep working, or recovery gets worse rather than better.
func TestNodeDerivedWritesSurviveAFullJournalDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	injectJournalFault(t, &faultDurability{writeErr: syscall.ENOSPC})
	store := openJournalledStore(t, path)

	if err := store.PutRouteSuspension(context.Background(), domain.RouteSuspension{
		ScopeKind: "deployment", ScopeKey: "dep_1", Reason: "invalid_credential",
		ObservedAt: time.Now().UTC(), Indefinite: true,
	}); err != nil {
		t.Fatalf("a node-derived write needed the journal disk: %v", err)
	}
}

// TestTheJournalIsIntactAfterARefusedFrame.
//
// A failed append must not leave a torn frame that the next open has to repair,
// and must not advance the chain. Otherwise the disk filling once would leave
// every later frame chained onto something that was never written.
func TestTheJournalIsIntactAfterARefusedFrame(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	// Let the attach and the first write through, then fail.
	fault := &faultDurability{writeErr: syscall.ENOSPC, after: 1}
	injectJournalFault(t, fault)
	store := openJournalledStore(t, path)

	if _, err := store.PutProject(context.Background(), domain.Project{
		ID: "prj_before", Name: "before", Enabled: true,
	}, 0, nil); err != nil {
		t.Fatal(err)
	}
	before, err := store.MetadataJournalState()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutProject(context.Background(), domain.Project{
		ID: "prj_refused", Name: "refused", Enabled: true,
	}, 0, nil); err == nil {
		t.Fatal("the second write succeeded despite a full disk")
	}
	journalPath := store.MetadataJournalPath()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// The file still verifies, and its head is where the successful write left
	// it. A torn tail would be repaired silently on reopen, which is right for
	// a crash and would be hiding a chain that never advanced here.
	metadatajournal.WrapDurability = nil
	head, err := metadatajournal.Verify(journalPath, testJournalKey())
	if err != nil {
		t.Fatalf("a refused append damaged the journal: %v", err)
	}
	if head.Sequence != before.Sequence {
		t.Fatalf("the chain advanced to %d on a write that failed; it was at %d",
			head.Sequence, before.Sequence)
	}
	// And the instance reopens with the successful write intact.
	reopened := openJournalledStore(t, path)
	if _, err := reopened.GetProject(context.Background(), "prj_before"); err != nil {
		t.Fatalf("the write that succeeded before the disk filled is gone: %v", err)
	}
	if _, err := reopened.GetProject(context.Background(), "prj_refused"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the refused write survived: %v", err)
	}
}
