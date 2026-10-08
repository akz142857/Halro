//go:build linux

package scratchstate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestScratchBlockBankActualENOSPCPoisonsCandidateAndKeepsFees(t *testing.T) {
	if os.Getenv("HALRO_SCRATCH_BLOCK_ENOSPC") != "1" {
		t.Skip("opt-in isolated actual ext4 write-failure fixture")
	}
	root := os.Getenv("HALRO_EXT4_ROOT")
	if root != "/halro-fixture/data" {
		t.Fatal("requires isolated ext4")
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC {
		t.Fatal("actual filesystem is not ext4", err)
	}
	s, manager := testStore(t, 8<<20)
	ctx := context.Background()
	if err := s.Update(ctx, func(tx *Transaction) error { return tx.Put([]byte("existing"), []byte("authenticated")) }); err != nil {
		t.Fatal(err)
	}
	before := manager.Snapshot().Used
	// This separate inode represents an external writer. It receives none of
	// the Store's permissions, fees or namespace retirement authority.
	filler, err := os.OpenFile(filepath.Join(t.TempDir(), "outside-fill"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	var block [1 << 20]byte
	for {
		n, err := filler.Write(block[:])
		if err != nil || n != len(block) {
			if !errors.Is(err, syscall.ENOSPC) && !errors.Is(err, io.ErrShortWrite) {
				t.Fatal("filler did not reach actual ENOSPC", n, err)
			}
			break
		}
	}
	// A large failed write can leave a few smaller allocatable blocks. Use
	// actual 4KiB writes to consume them instead of mistaking ENOSPC at the
	// previous write size for an empty filesystem.
	for {
		n, err := filler.Write(block[:4096])
		if err != nil || n != 4096 {
			if !errors.Is(err, syscall.ENOSPC) {
				t.Fatal("small filler did not reach actual ENOSPC", n, err)
			}
			break
		}
	}
	if err := filler.Close(); err != nil {
		t.Fatal("actual filler Close is unknown", err)
	}
	if err := unix.Statfs(root, &fs); err != nil || fs.Bavail > 1 {
		t.Fatal("actual filesystem still has material space", fs.Bavail, err)
	}
	err = s.Update(ctx, func(tx *Transaction) error {
		if _, found, err := tx.Get([]byte("existing")); err != nil || !found {
			return errors.Join(ErrCorrupt, err)
		}
		return tx.Put([]byte("new"), bytes.Repeat([]byte("v"), 1<<20))
	})
	if !errors.Is(err, syscall.ENOSPC) || s.fault == nil || s.key == ([32]byte{}) || manager.Snapshot().Used != before {
		t.Fatal("actual failed write lost its candidate or fees", err, manager.Snapshot())
	}
	if err := s.View(ctx, func(*Transaction) error { t.Fatal("failed candidate exported a root"); return nil }); !errors.Is(err, ErrPoisoned) {
		t.Fatal(err)
	}
	if err := s.Update(ctx, func(*Transaction) error { t.Fatal("failed candidate accepted another write"); return nil }); !errors.Is(err, ErrPoisoned) {
		t.Fatal(err)
	}
	// The captured private candidate may refund only after real FD/namespace
	// retirement; external filler remains present throughout that retirement.
	if err := s.Discard(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filler.Name()); err != nil {
		t.Fatal("candidate retirement touched external filler", err)
	}
}
