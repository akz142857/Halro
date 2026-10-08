//go:build linux

package authenticatedstate

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

func TestPhysicalFullFilesystemPreacceptRefusesWithoutChangingReceiptAndPaidTerminalCompletes(t *testing.T) {
	requirePhysicalReceiptExt4(t)
	s, options := testStoreWithoutCleanup(t, 3)
	ctx := context.Background()
	nativePath := filepath.Join(filepath.Dir(s.dir), "native")
	if err := os.WriteFile(nativePath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	native, verify := receiptNativeFixture(t, nativePath)
	if _, err := s.EnablePhysicalTerminalQualified(ctx, nextCut(99), native, verify); err != nil {
		t.Fatal(err)
	}
	w, err := s.ReservePhysicalTerminals(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	priorEnd, priorDebt := s.physical.allocatedEnd, s.physical.debt
	priorSerial := s.physical.receipt.claims.Serial
	before, err := s.pack.Stat()
	if err != nil {
		t.Fatal(err)
	}
	fees := options.Budget.Snapshot().Used
	filler := exhaustSealPhysicalExt4(t, filepath.Dir(s.dir))
	for attempt := 0; attempt < 3; attempt++ {
		if attempt == 2 {
			f, err := os.OpenFile(filler, os.O_RDWR|syscall.O_SYNC, 0600)
			if err != nil {
				t.Fatal(err)
			}
			info, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if err := errors.Join(f.Truncate(info.Size()-4096), f.Sync(), f.Close()); err != nil {
				t.Fatal(err)
			}
			var fs unix.Statfs_t
			if err := unix.Statfs(filler, &fs); err != nil || fs.Bavail == 0 || fs.Bavail >= uint64(s.appendLimit/fs.Bsize) {
				t.Fatal("expected real nonzero but insufficient free blocks", fs.Bavail, err)
			}
			t.Logf("actual insufficient positive Bavail=%d", fs.Bavail)
		}
		if _, err := s.ReservePhysicalTerminals(ctx, 1); !errors.Is(err, resourcebudget.ErrPhysicalAllocationNoWrite) || !errors.Is(err, syscall.ENOSPC) || errors.Is(err, resourcebudget.ErrPhysicalAllocationUncertain) {
			t.Fatal("new work did not refuse before allocation", err)
		}
		if s.physical.failure != nil || s.fault() != nil || s.physical.allocatedEnd != priorEnd || s.physical.debt != priorDebt || options.Budget.Snapshot().Used != fees {
			t.Fatal("known refusal lost original owner, boundary or fee")
		}
		raw, _, err := readFixed(s.receiptPath(s.physical.receipt.bank), physicalReceiptBytes)
		if err != nil {
			t.Fatal(err)
		}
		claims, err := decodePhysicalReceipt(raw, s.key)
		if err != nil || claims.COWEnd != priorEnd || claims.Debt != priorDebt || claims.Serial <= priorSerial {
			t.Fatal("restored MAC receipt lost original boundary", claims.COWEnd, claims.Serial, err)
		}
	}
	after, err := s.pack.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || before.Sys().(*syscall.Stat_t).Blocks != after.Sys().(*syscall.Stat_t).Blocks {
		t.Fatal("preaccept changed actual pack", err)
	}
	if err := resourcebudget.CheckPhysicalTail(s.pack, int64(s.physicalExtent), priorEnd); err != nil {
		t.Fatal("preaccept allocated an unrecorded tail", err)
	}
	// Reclaim the single released block as the independent foreign writer.
	f, err := os.OpenFile(filler, os.O_WRONLY|os.O_APPEND|syscall.O_SYNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := f.Write(make([]byte, 4096)); err != nil || n != 4096 {
		t.Fatal("foreign refill failed", n, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if fs := requireSealPhysicalExt4(t, filler); fs.Bavail != 0 {
		t.Fatal("original terminal must run at real zero blocks", fs.Bavail)
	}
	// Fallocate over an already complete target is still valid at actual Bavail0.
	if err := resourcebudget.AllocatePhysicalRange(s.pack, int64(s.physicalExtent), priorDebt); err != nil {
		t.Fatal("original prepaid target refused", err)
	}
	u, err := s.PreparePhysicalUpdate(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	_, commitErr := s.Update(u.Bind(ctx), nextCut(1), func(tx *Transaction) error { return tx.Put([]byte("key_000000"), []byte("original accepted terminal")) })
	if err := u.Finish(true, commitErr); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if s.physical.debt != 0 || s.physical.failure != nil {
		t.Fatal("original terminal did not finish", s.physical.debt, s.physical.failure)
	}
	if err := os.Remove(filler); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Discard(); err != nil {
		t.Fatal(err)
	}
}
