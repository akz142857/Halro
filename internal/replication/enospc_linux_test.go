//go:build linux

package replication

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// The opt-in fixture is safe only inside a small dedicated tmpfs. It never
// fills the host filesystem and refuses an ordinary or oversized mount.
func TestOrderingActualLinuxENOSPCPreservesHeadAndReopens(t *testing.T) {
	root := os.Getenv("HALRO_ORDERING_ENOSPC_ROOT")
	if root == "" {
		t.Skip("requires explicit dedicated small Linux tmpfs")
	}
	if !filepath.IsAbs(root) {
		t.Fatal("ENOSPC root must be absolute")
	}
	named, err := os.Lstat(root)
	if err != nil || !named.IsDir() || named.Mode()&os.ModeSymlink != 0 {
		t.Fatal("ENOSPC root must be a real directory", err)
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil {
		t.Fatal(err)
	}
	const tmpfsMagic = 0x01021994
	if fs.Type != tmpfsMagic || uint64(fs.Blocks)*uint64(fs.Bsize) > 32<<20 {
		t.Fatal("refusing to fill a non-tmpfs or a mount over 32 MiB")
	}
	directory, err := os.MkdirTemp(root, "halro-ordering-enospc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "ordering.log")
	key := make([]byte, 32)
	header := OrderingHeader{ClusterID: "c", Incarnation: strings.Repeat("i", 15)}
	log, err := OpenOrderingJournal(path, key, header, 0, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	record := func(index uint64) OrderingRecord {
		out := OrderingRecord{Kind: KindData, Store: StoreLedger, Index: index, Term: 1, StoreGeneration: 1, StoreSequenceFirst: index - 1, StoreSequenceLast: index - 1, FrameDigest: sha256.Sum256([]byte("fixture-source-frame"))}
		if index == 1 {
			out = OrderingRecord{Kind: KindLeadershipEstablished, Store: StoreNone, Index: index, Term: 1, FrameDigest: out.FrameDigest}
		}
		return out
	}
	var beforeSize int64
	for index := uint64(1); index <= 4096; index++ {
		if _, err := log.Append(record(index)); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size()%fs.Bsize == 0 {
			beforeSize = info.Size()
			break
		}
	}
	if beforeSize == 0 {
		t.Fatal("fixture did not align its complete prefix to physical blocks")
	}
	beforeIndex, beforeTerm, beforeMAC := log.Head()
	fillerPath := filepath.Join(directory, "fill.tmpfs")
	filler, err := os.OpenFile(fillerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 64<<10)
	var filled int64
	for {
		n, err := filler.Write(chunk)
		filled += int64(n)
		if err != nil {
			if !errors.Is(err, syscall.ENOSPC) {
				_ = filler.Close()
				t.Fatal(err)
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
	if err := unix.Statfs(root, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Bavail != 0 {
		t.Fatal("fixture did not exhaust physical blocks", fs.Bavail)
	}
	_, appendErr := log.Append(record(beforeIndex + 1))
	if !errors.Is(appendErr, syscall.ENOSPC) {
		t.Fatal("ordering append did not encounter actual ENOSPC", appendErr)
	}
	if index, term, mac := log.Head(); index != beforeIndex || term != beforeTerm || mac != beforeMAC {
		t.Fatal("failed append advanced a durable ordering head", index, term, mac)
	}
	if _, err := log.Append(record(beforeIndex + 1)); err == nil {
		t.Fatal("uncertain ordering writer accepted a later append")
	}
	partial, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fillerPath); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenOrderingJournal(path, key, header, beforeIndex, beforeMAC)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if index, term, mac := reopened.Head(); index != beforeIndex || term != beforeTerm || mac != beforeMAC {
		t.Fatal("reopen changed authenticated ordering prefix", index, term, mac)
	}
	if _, err := reopened.Append(record(beforeIndex + 1)); err != nil {
		t.Fatal(err)
	}
	if index, _, _ := reopened.Head(); index != beforeIndex+1 {
		t.Fatal("recovered ordering writer lost the new record", index)
	}
	t.Logf("actual tmpfs ENOSPC: filled=%d, prior_native=%d, failed_native_size=%d, free_blocks_at_failure=%d, append_error=%v", filled, beforeSize, partial.Size(), fs.Bavail, appendErr)
}
