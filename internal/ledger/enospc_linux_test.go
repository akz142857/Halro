//go:build linux

package ledger

import (
	"context"
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
func TestLedgerActualLinuxENOSPCPreservesPrefixAndReopensPartialTail(t *testing.T) {
	root := os.Getenv("HALRO_LEDGER_ENOSPC_ROOT")
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
	directory, err := os.MkdirTemp(root, "halro-ledger-enospc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "ledger.wal")
	key := make([]byte, 32)
	log, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	event := validReservation("durable-before-enospc", "attempt-before")
	if _, err := log.Append(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	before, beforeHash, ok := log.ChainHead()
	if !ok {
		t.Fatal("initial prefix is not authenticated")
	}
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
	wide := event
	wide.EventID = "refused-enospc"
	wide.EventID = strings.Repeat("x", 48<<10)
	_, appendErr := log.Append(context.Background(), wide)
	if !errors.Is(appendErr, syscall.ENOSPC) {
		t.Fatal("native append did not encounter actual ENOSPC", appendErr)
	}
	if actual, hash, _ := log.ChainHead(); actual != before || hash != beforeHash {
		t.Fatal("failed write credited a durable head", actual, before)
	}
	if _, err := log.Append(context.Background(), event); err == nil {
		t.Fatal("poisoned writer accepted a later append")
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
	reopened, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if actual, hash, _ := reopened.ChainHead(); actual != before || hash != beforeHash {
		t.Fatal("reopen changed the complete authenticated prefix", actual, before)
	}
	wide.EventID = "accepted-after-space-return"
	if _, err := reopened.Append(context.Background(), wide); err != nil {
		t.Fatal(err)
	}
	if actual, _, _ := reopened.ChainHead(); actual.Sequence != before.Sequence+1 {
		t.Fatal("recovered writer lost the new complete fact", actual)
	}
	t.Logf("actual tmpfs ENOSPC: filled=%d, prior_native=%d, failed_native_size=%d, free_blocks_at_failure=%d, append_error=%v", filled, before.Offset, partial.Size(), fs.Bavail, appendErr)
}
