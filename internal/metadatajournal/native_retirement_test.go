package metadatajournal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
)

// These fixtures deliberately retain uncertain physical ownership. A cleanup
// which retried Close or removed the pathname would erase the tested contract.
func retirementJournal(t *testing.T) *Log {
	t.Helper()
	b := testBinding()
	l, err := StartBoundEpoch(context.Background(), filepath.Join(t.TempDir(), "metadata.journal"), testKey(), EpochHeader{Epoch: 9, PreviousEpoch: 8, Reason: "retirement", Baseline: &b, FileRevision: 1}, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestMetadataActualWriterCloseDebtPreservesAdmissionAndReceipt(t *testing.T) {
	l := retirementJournal(t)
	a := &metadataAppendAdmissionFixture{}
	l.uncertainAdmissions = []AppendAdmission{a}
	actual := l.file
	if err := actual.Close(); err != nil {
		t.Fatal(err)
	}
	err := l.Close()
	if !errors.Is(err, ownedfile.ErrRetirement) || l.file != actual || a.releases != 0 || len(l.key) == 0 || l.created == nil {
		t.Fatal("actual Close debt lost ownership", err, a.releases)
	}
	for range 3 {
		if !errors.Is(l.Close(), ownedfile.ErrRetirement) || !errors.Is(l.Discard(), ownedfile.ErrRetirement) || !errors.Is(l.HealthError(), ownedfile.ErrRetirement) {
			t.Fatal("repeat cleanup erased debt")
		}
	}
	if _, err := l.Append([]Op{put("projects", "refused", "v")}); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
	if _, err := l.InspectBoundContext(context.Background(), nil); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
	if err := l.ReplayRange(0, 0, nil); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.path); err != nil || a.releases != 0 {
		t.Fatal("unknown close must preserve original inode and fee", err)
	}
}

func TestMetadataConstructorActualCloseFailureIsTyped(t *testing.T) {
	l := retirementJournal(t)
	head := l.Head()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	var actual *os.File
	got, err := openExpectedWithFactory(context.Background(), l.path, testKey(), false, &head.Baseline, head.FileRevision, 64<<20, &head, func(file *os.File) DurabilityWriter {
		actual = file
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return nil
	})
	if got != nil || actual == nil || !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(got, err)
	}
	if _, err := os.Stat(l.path); err != nil {
		t.Fatal("failed borrowed constructor must preserve original", err)
	}
}

func TestMetadataTrimActualOldDescriptorCloseDebtKeepsPublishedInode(t *testing.T) {
	l := retirementJournal(t)
	for range 2 {
		if _, err := l.Append([]Op{put("projects", "p", "v")}); err != nil {
			t.Fatal(err)
		}
	}
	head := l.Head()
	state := nativeTrimState(t, l, 1)
	actual := l.file
	got, err := l.trimBoundContext(context.Background(), head, state, func(phase string) {
		if phase == "journal-renamed" {
			if err := actual.Close(); err != nil {
				t.Fatal(err)
			}
		}
	})
	if got != (Head{}) || !errors.Is(err, ownedfile.ErrRetirement) || l.file != actual || l.created == nil {
		t.Fatal("rewrite debt erased actual old owner", got, err)
	}
	if !errors.Is(l.Discard(), ownedfile.ErrRetirement) {
		t.Fatal("discard accepted uncertain physical rewrite")
	}
	if _, err := os.Stat(l.path); err != nil {
		t.Fatal("published actual inode lost", err)
	}
	if _, err := l.Append([]Op{put("projects", "refused", "v")}); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
}

func TestMetadataCloseJoinsActualPreparedPublication(t *testing.T) {
	l := retirementJournal(t)
	entered, release := make(chan struct{}), make(chan struct{})
	appendDone, closeDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := l.AppendPreparedContext(context.Background(), []Op{put("projects", "p", "v")}, func(_ Record, durable func() error) error {
			close(entered)
			<-release
			return durable()
		})
		appendDone <- err
	}()
	<-entered
	go func() { closeDone <- l.Close() }()
	select {
	case err := <-closeDone:
		close(release)
		<-appendDone
		t.Fatal("Close escaped actual native publication", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-appendDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil || l.file != nil || l.durability != nil {
		t.Fatal("known Close did not retire descriptor", err)
	}
}

func TestMetadataDiscardActualDirectoryCloseDebtCannotRetryToSuccess(t *testing.T) {
	l := retirementJournal(t)
	calls := 0
	l.syncRetiredDirectory = func(path string) error {
		calls++
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		return ownedfile.CloseFile(f)
	}
	err := l.Discard()
	if !errors.Is(err, ownedfile.ErrRetirement) || calls != 1 {
		t.Fatal(err, calls)
	}
	for range 3 {
		if l.Discard() != err || l.Close() != err || !errors.Is(l.HealthError(), ownedfile.ErrRetirement) || calls != 1 {
			t.Fatal("repeat cleanup erased directory Close debt")
		}
	}
	if _, err := os.Stat(l.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("only the owned native inode was retired", err)
	}
}
