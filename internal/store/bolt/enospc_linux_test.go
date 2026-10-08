//go:build linux

package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"golang.org/x/sys/unix"
)

func TestMetadataProjectionActualLinuxENOSPCRequiresReopen(t *testing.T) {
	root := os.Getenv("HALRO_METADATA_STORE_ENOSPC_ROOT")
	if root == "" {
		t.Skip("requires explicit dedicated small Linux tmpfs")
	}
	named, err := os.Lstat(root)
	if !filepath.IsAbs(root) || err != nil || !named.IsDir() || named.Mode()&os.ModeSymlink != 0 {
		t.Fatal("ENOSPC root must be an absolute real directory", err)
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type != 0x01021994 || uint64(fs.Blocks)*uint64(fs.Bsize) > 32<<20 {
		t.Fatal("refusing to fill a non-tmpfs or a mount over 32 MiB")
	}
	directory, err := os.MkdirTemp(root, "halro-metadata-db-enospc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "metadata.db")
	s := openJournalledStore(t, path)
	ctx := context.Background()
	if _, err := s.PutProject(ctx, domain.Project{ID: "prior", Name: "prior", Enabled: true}, 0, nil); err != nil {
		t.Fatal(err)
	}
	before := s.journal.Head()
	// Reserve physical native blocks while preserving the authenticated length.
	// The journal can fsync the next fact, but bbolt has no blocks to commit it.
	native, err := os.OpenFile(s.MetadataJournalPath(), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Fallocate(int(native.Fd()), unix.FALLOC_FL_KEEP_SIZE, 0, 128<<10); err != nil {
		_ = native.Close()
		t.Fatal(err)
	}
	if err := errors.Join(native.Sync(), native.Close()); err != nil {
		t.Fatal(err)
	}
	fillerPath := filepath.Join(directory, "fill.tmpfs")
	filler, err := os.OpenFile(fillerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 64<<10)
	var filled int64
	for {
		n, writeErr := filler.Write(chunk)
		filled += int64(n)
		if writeErr != nil {
			if !errors.Is(writeErr, syscall.ENOSPC) {
				_ = filler.Close()
				t.Fatal(writeErr)
			}
			break
		}
		if filled > 32<<20 {
			_ = filler.Close()
			t.Fatal("dedicated volume exceeded guarded limit")
		}
	}
	if err := errors.Join(filler.Sync(), filler.Close()); err != nil {
		t.Fatal(err)
	}
	if err := unix.Statfs(root, &fs); err != nil || fs.Bavail != 0 {
		t.Fatal("fixture did not exhaust physical blocks", fs.Bavail, err)
	}
	project := domain.Project{ID: "durable-not-applied", Name: strings.Repeat("p", 48<<10), Enabled: true}
	raw, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	commitErr := s.update(func(tx *Tx) error { return tx.Bucket(bucketProjects).Put([]byte(project.ID), raw) })
	if !errors.Is(commitErr, syscall.ENOSPC) {
		t.Fatal("projection commit did not encounter actual ENOSPC", commitErr)
	}
	if head := s.journal.Head(); head.Sequence != before.Sequence+1 {
		t.Fatal("fixture did not persist the native fact before failed commit", head, before)
	}
	// A live process cannot read stale authority or publish a later transaction
	// over the missing fact. Recovery must close, replay and reopen admission.
	if _, err := s.GetProject(ctx, "prior"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatal("failed native/projection boundary served stale metadata", err)
	}
	called := false
	if err := s.update(func(*Tx) error { called = true; return nil }); err == nil || called {
		t.Fatal("failed native/projection boundary admitted a later write", err, called)
	}
	if err := os.Remove(fillerPath); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state, err := reopened.AttachMetadataJournal(testJournalKey(), "actual ENOSPC recovery")
	if err != nil || state.Replayed != 1 || state.Applied != before.Sequence+1 {
		t.Fatal("reopen did not reconcile the durable fact", state, err)
	}
	actual, err := reopened.GetProject(ctx, project.ID)
	if err != nil || actual.Name != project.Name {
		t.Fatal("native recovery lost the durable project", err)
	}
	t.Logf("actual tmpfs ENOSPC: filled=%d, free_blocks_at_failure=%d, commit_error=%v; native advanced by one, failed projection retired and reopen replayed it", filled, fs.Bavail, commitErr)
}
