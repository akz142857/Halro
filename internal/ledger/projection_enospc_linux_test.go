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

func TestLedgerProjectionActualLinuxENOSPCDiscardsAndRebuilds(t *testing.T) {
	root := os.Getenv("HALRO_LEDGER_ENOSPC_ROOT")
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
	directory, err := os.MkdirTemp(root, "halro-projection-enospc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	ctx := context.Background()
	path := filepath.Join(directory, "ledger.wal")
	log, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	first := validReservation("initial", "attempt-initial")
	if _, err := log.Append(ctx, first); err != nil {
		t.Fatal(err)
	}
	p, err := log.BuildFreshProjection(ctx, filepath.Join(directory, "reference.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	before, beforeHash, err := p.ReadPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	balanceKey := BalanceKey{first.ProjectID, first.PeriodID, first.PeriodTimezoneVersion}
	beforeBalance, err := p.ReadBalance(ctx, balanceKey)
	if err != nil {
		t.Fatal(err)
	}
	next := validReservation("next", "attempt-next")
	next.RequestID = "request-next"
	next.RequestedModel = strings.Repeat("m", 48<<10)
	record, _, err := log.AppendRecordWithReceipt(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	nativeBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fillerPath := filepath.Join(directory, "fill.tmpfs")
	filler, err := os.OpenFile(fillerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	var filled int64
	for {
		n, writeErr := filler.Write(make([]byte, 64<<10))
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
	applyErr := p.Apply(ctx, record)
	if !errors.Is(applyErr, syscall.ENOSPC) {
		t.Fatal("projection commit did not encounter actual ENOSPC", applyErr)
	}
	if position, hash, err := p.ReadPosition(ctx); err != nil || position != before || hash != beforeHash {
		t.Fatal("failed commit changed the selected position", position, err)
	}
	if balance, err := p.ReadBalance(ctx, balanceKey); err != nil || balance != beforeBalance {
		t.Fatal("failed commit credited accounting state", balance, err)
	}
	if err := os.Remove(fillerPath); err != nil {
		t.Fatal(err)
	}
	if err := p.Discard(); err != nil {
		t.Fatal(err)
	}
	fresh, err := log.BuildFreshProjection(ctx, filepath.Join(directory, "rebuilt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Discard()
	position, hash, err := fresh.ReadPosition(ctx)
	if err != nil || position.Sequence != record.Sequence || hash != record.Hash {
		t.Fatal("fresh rebuild did not include both native facts", position, err)
	}
	if balance, err := fresh.ReadBalance(ctx, balanceKey); err != nil || balance.ReservedMicrosUSD != 200 {
		t.Fatal("fresh rebuild lost the durable reservation", balance, err)
	}
	if nativeAfter, err := os.ReadFile(path); err != nil || string(nativeAfter) != string(nativeBefore) {
		t.Fatal("projection recovery modified native evidence", err)
	}
	t.Logf("actual tmpfs ENOSPC: filled=%d, free_blocks_at_failure=%d, apply_error=%v; failed candidate discarded and full native rebuild succeeded", filled, fs.Bavail, applyErr)
}
