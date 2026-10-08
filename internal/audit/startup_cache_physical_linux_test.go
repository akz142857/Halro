//go:build linux

package audit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

func TestStartupCachePreparedPhysicalTargetActualENOSPC(t *testing.T) {
	if os.Getenv("HALRO_EXT4_ROOT") == "" {
		t.Skip("isolated ext4 fixture required")
	}
	f := newStartupCacheFixture(t, 3)
	dir := filepath.Dir(f.path)
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC || fs.Blocks*uint64(fs.Bsize) > 512<<20 {
		t.Fatal(fs, err)
	}
	manager := f.options.CacheBudget
	disk, err := manager.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: MaxStartupCacheBytes})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "physical-cache.pending")
	target, err := resourcebudget.PreparePhysicalCandidate(context.Background(), path, MaxStartupCacheBytes, manager, disk)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	filler, err := os.OpenFile(filepath.Join(dir, "foreign-filler"), os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_SYNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	size := len(chunk)
	for {
		_, err := filler.Write(chunk[:size])
		if err == nil {
			continue
		}
		if !errors.Is(err, syscall.ENOSPC) {
			t.Fatal(err)
		}
		if size > 4096 {
			size = 4096
			continue
		}
		break
	}
	if err := filler.Close(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Statfs(dir, &fs); err != nil || fs.Bavail != 0 {
		t.Fatal("fixture not full", fs.Bavail, err)
	}
	published, err := f.cache.PersistNew(context.Background(), path, StartupCachePersistOptions{Guard: f.selected.Guard, Budget: manager, DiskReservation: disk, Identity: f.selected.Identity, Head: f.cache.Head(), PhysicalCandidate: target})
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.Stat(path)
	if err != nil || !os.SameFile(original, actual) {
		t.Fatal("cache created a different target inode", err)
	}
	if err := os.Remove(filler.Name()); err != nil {
		t.Fatal(err)
	}
	if err := published.Discard(); err != nil {
		t.Fatal(err)
	}
	if err := disk.Release(); err != nil {
		t.Fatal(err)
	}
}
