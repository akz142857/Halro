//go:build linux

package metadatajournal

import (
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
func TestMetadataActualLinuxENOSPCPreservesPrefixAndReopensPartialTail(t *testing.T) {
	root := os.Getenv("HALRO_METADATA_ENOSPC_ROOT")
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
	directory, err := os.MkdirTemp(root, "halro-metadata-enospc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "metadata.journal")
	key := make([]byte, 32)
	log, err := StartEpoch(path, key, EpochHeader{Epoch: 1, Reason: "enospc-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	event := []Op{put("projects", "durable-before-enospc", "v")}
	if _, err := log.Append(event); err != nil {
		t.Fatal(err)
	}
	before := log.Head()
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
	wide := []Op{put("projects", "refused-enospc", strings.Repeat("x", 48<<10))}
	_, appendErr := log.Append(wide)
	if !errors.Is(appendErr, syscall.ENOSPC) {
		t.Fatal("native append did not encounter actual ENOSPC", appendErr)
	}
	if actual := log.Head(); actual != before {
		t.Fatal("failed write credited a durable head", actual, before)
	}
	if _, err := log.Append(event); err == nil {
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
	reopened, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if actual := reopened.Head(); actual != before {
		t.Fatal("reopen changed the complete authenticated prefix", actual, before)
	}
	wide = []Op{put("projects", "accepted-after-space-return", "v")}
	if _, err := reopened.Append(wide); err != nil {
		t.Fatal(err)
	}
	if actual := reopened.Head(); actual.Sequence != before.Sequence+1 {
		t.Fatal("recovered writer lost the new complete fact", actual)
	}
	t.Logf("actual tmpfs ENOSPC: filled=%d, prior_native=%d, failed_native_size=%d, free_blocks_at_failure=%d, append_error=%v", filled, before.Offset, partial.Size(), fs.Bavail, appendErr)
}
