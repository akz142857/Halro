package bolt

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

func TestMetadataDataFileCeilingPreservesDurableFactAndRequiresRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "metadata.db")
	s := openJournalledStore(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err = OpenWithDiskLimit(path, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.AttachMetadataJournal(testJournalKey(), "file ceiling"); err != nil {
		t.Fatal(err)
	}
	before := s.journal.Head()
	project := domain.Project{ID: "durable", Name: "durable", Enabled: true, AllowedModels: []string{strings.Repeat("m", int(info.Size())+64<<10)}}
	_, err = s.PutProject(ctx, project, 0, nil)
	if !errors.Is(err, ErrMetadataStoreDiskLimit) || !errors.Is(err, ErrMetadataRecoveryRequired) || s.journal.Head().Sequence != before.Sequence+1 {
		t.Fatal("file ceiling did not preserve and latch durable-but-unapplied fact", err)
	}
	actual, err := os.Stat(path)
	if err != nil || actual.Size() > info.Size() {
		t.Fatal("metadata data file escaped its configured ceiling", actual, err)
	}
	if _, err := s.GetProject(ctx, project.ID); !errors.Is(err, ErrMetadataRecoveryRequired) {
		t.Fatal("quota failure served stale metadata", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := OpenWithDiskLimit(path, info.Size()+8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	state, err := next.AttachMetadataJournal(testJournalKey(), "capacity raised")
	if err != nil || state.Replayed != 1 {
		t.Fatal("new owner did not authenticate and reconcile retained native fact", state, err)
	}
	got, err := next.GetProject(ctx, project.ID)
	if err != nil || len(got.AllowedModels) != 1 || got.AllowedModels[0] != project.AllowedModels[0] {
		t.Fatal("capacity recovery lost the native fact", err)
	}
}

func TestMetadataDataFileCeilingRejectsExistingOversizeWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	s := openJournalledStore(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, open := range map[string]func(string, int64) (*Store, error){
		"standalone": OpenWithDiskLimit, "primary": OpenPrimaryWithDiskLimit,
		"replica": OpenReplicaWithDiskLimit, "readonly": OpenReadOnlyWithDiskLimit,
		"publication": OpenForWholeFilePublishWithDiskLimit,
	} {
		t.Run(name, func(t *testing.T) {
			if store, err := open(path, int64(len(before))-1); store != nil || !errors.Is(err, ErrMetadataStoreDiskLimit) {
				t.Fatal("oversized file was admitted", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
				t.Fatal("oversize refusal modified native metadata", err)
			}
		})
	}
}

func TestMetadataDataFileCeilingPreservesNoCreateAndRejectsNegative(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "absent", "metadata.db")
	if _, err := OpenWithDiskLimit(path, -1); !errors.Is(err, ErrMetadataStoreDiskLimit) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("negative limit created a native directory", err)
	}
	for _, open := range []func(string, int64) (*Store, error){OpenPrimaryWithDiskLimit, OpenReplicaWithDiskLimit, OpenReadOnlyWithDiskLimit} {
		if store, err := open(path, 8<<20); store != nil || !errors.Is(err, os.ErrNotExist) {
			t.Fatal("seeded/read-only open created missing metadata", err)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing seed was silently created", err)
	}
}
