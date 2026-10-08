//go:build linux

package authenticatedstate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestPhysicalNoDebtRolloverRetainsExactControls(t *testing.T) {
	requirePhysicalReceiptExt4(t)
	for _, name := range []string{"rollover", "wrong-cut", "foreign-control", "wrong-native-hash"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			creator, o := testStoreWithoutCleanup(t, 3)
			selected, err := creator.RootIdentity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = creator.Close(); err != nil {
				t.Fatal(err)
			}
			nativePath := filepath.Join(filepath.Dir(creator.dir), "native")
			if err = os.WriteFile(nativePath, nil, 0600); err != nil {
				t.Fatal(err)
			}
			f := physicalReceiptCrashFixture{Directory: creator.dir, Native: nativePath, Root: selected, Selection: nextCut(99)}
			raw, err := json.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(filepath.Dir(creator.dir), "fixture.json")
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPhysicalReceiptSIGKILLHelper$", "-test.count=1")
			child.Env = append(os.Environ(), "HALRO_PHYSICAL_RECEIPT_FIXTURE="+path, "HALRO_PHYSICAL_RECEIPT_PHASE=physical-pool-allocated")
			output, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("actual owned preparation crash absent: %v %s", err, output)
			}
			if name == "foreign-control" {
				path := filepath.Join(creator.dir, "terminal.physical.0")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			var controls [2]os.FileInfo
			for i := range controls {
				controls[i], err = os.Lstat(filepath.Join(creator.dir, "terminal.physical."+string(rune('0'+i))))
				if err != nil {
					t.Fatal(err)
				}
			}
			o.ExpectedRoot = &selected
			s, err := OpenStore(ctx, creator.dir, o)
			if err != nil {
				t.Fatal(err)
			}
			native, verify := receiptNativeFixture(t, nativePath)
			newSelection := nextCut(100)
			window, _, found, err := s.RestorePhysicalTerminalForRecovery(ctx, newSelection, native, o.ExpectedCut, verify)
			if !errors.Is(err, ErrSealIneligible) || found || window != nil || s.physical != nil {
				t.Fatal("normal restore accepted older common selection", found, err)
			}
			cut := o.ExpectedCut
			if name == "wrong-cut" {
				cut = nextCut(3)
			}
			if name == "wrong-native-hash" {
				native.Hash = nextCut(7)
			}
			_, rollErr := s.EnablePhysicalTerminalAfterQualifiedNoDebt(ctx, newSelection, native, cut, verify)
			if (name == "rollover") != (rollErr == nil) {
				t.Fatal("independent root qualification result", rollErr)
			}
			for i, before := range controls {
				after, err := os.Lstat(s.receiptPath(i))
				if err != nil || !os.SameFile(before, after) {
					t.Fatal("rollover replaced/deleted captured control inode", err)
				}
			}
			if name == "rollover" {
				if s.physical.receipt.claims.Selection != newSelection || s.physical.debt != 0 || s.physical.windows != 0 {
					t.Fatal("new receipt still has old selection/debt")
				}
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				_ = s.Close()
			}
		})
	}
}
