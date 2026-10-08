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

	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

func TestPhysicalRestoreReceiptBindsActualRootAndTargetCoverage(t *testing.T) {
	requirePhysicalReceiptExt4(t)
	for _, name := range []string{"restore", "foreign-pool", "foreign-root", "missing-pool-extent", "wrong-selection", "legacy-no-root-authority"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			creator, o := testStoreWithoutCleanup(t, 3)
			root, err := creator.RootIdentity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := creator.Close(); err != nil {
				t.Fatal(err)
			}
			nativePath := filepath.Join(filepath.Dir(creator.dir), "native")
			if err := os.WriteFile(nativePath, nil, 0600); err != nil {
				t.Fatal(err)
			}
			fixture := physicalReceiptCrashFixture{Directory: creator.dir, Native: nativePath, Root: root, Selection: nextCut(99)}
			raw, err := json.Marshal(fixture)
			if err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(filepath.Dir(creator.dir), "fixture.json")
			if err := os.WriteFile(configPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPhysicalReceiptSIGKILLHelper$", "-test.count=1")
			child.Env = append(os.Environ(), "HALRO_PHYSICAL_RECEIPT_FIXTURE="+configPath, "HALRO_PHYSICAL_RECEIPT_PHASE=physical-pool-allocated")
			output, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("not actual captured target crash: %v %s", err, output)
			}
			pool := filepath.Join(creator.dir, "terminal-root-00")
			active := filepath.Join(creator.dir, "active.root")
			if name == "foreign-pool" || name == "foreign-root" {
				path := pool
				if name == "foreign-root" {
					path = active
				}
				contents, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, contents, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if name == "missing-pool-extent" {
				f, err := os.OpenFile(pool, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				punchErr := unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, 0, 4096)
				if err := errors.Join(punchErr, f.Truncate(0), f.Sync(), f.Close()); err != nil {
					t.Fatal(err)
				}
			}
			if name == "legacy-no-root-authority" {
				for bank := 0; bank < 2; bank++ {
					path := filepath.Join(creator.dir, "terminal.physical."+string(rune('0'+bank)))
					raw, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					claims, err := decodePhysicalReceipt(raw, o.Key)
					if err != nil {
						t.Fatal(err)
					}
					claims.COWRoot = [cowRootBytes]byte{}
					claims.COWRootFile = resourcebudget.PhysicalIdentity{}
					raw, err = encodePhysicalReceipt(claims, o.Key)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, raw, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			beforePool, err := os.Stat(pool)
			if err != nil {
				t.Fatal(err)
			}
			beforeRoot, err := os.Stat(active)
			if err != nil {
				t.Fatal(err)
			}
			o.ExpectedRoot = &root
			s, err := OpenStore(ctx, creator.dir, o)
			if err != nil {
				t.Fatal(err)
			}
			native, verify := receiptNativeFixture(t, nativePath)
			selection := fixture.Selection
			if name == "wrong-selection" {
				selection = nextCut(100)
			}
			window, _, found, restoreErr := s.RestorePhysicalTerminalForRecovery(ctx, selection, native, o.ExpectedCut, verify)
			if name == "restore" {
				if restoreErr != nil || !found || window.Count() != 1 {
					t.Fatal("actual target was not borrowed", found, restoreErr)
				}
				afterPool, err := os.Stat(pool)
				if err != nil || !os.SameFile(beforePool, afterPool) || afterPool.Size() != 0 {
					t.Fatal("restore created a replacement pool inode", err)
				}
				// Component caller models a completed NoDebt proof; Runtime uses its
				// independently audited actual RecoveryView before this retirement.
				if err := window.Close(); err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				return
			}
			if restoreErr == nil || found || window != nil {
				t.Fatal("unqualified target borrowed physical authority", found, restoreErr)
			}
			afterPool, err := os.Stat(pool)
			if err != nil || !os.SameFile(beforePool, afterPool) || afterPool.Size() != beforePool.Size() {
				t.Fatal("rejection changed pool", err)
			}
			afterRoot, err := os.Stat(active)
			if err != nil || !os.SameFile(beforeRoot, afterRoot) || afterRoot.Size() != beforeRoot.Size() {
				t.Fatal("rejection changed root", err)
			}
			beforeClose := o.Budget.Snapshot().Used.Disk
			if name == "wrong-selection" {
				if !errors.Is(restoreErr, ErrSealIneligible) || s.physical != nil {
					t.Fatal("selection mismatch lent an owner", restoreErr)
				}
				if err := s.Close(); err != nil || o.Budget.Snapshot().Used.Disk != beforeClose {
					t.Fatal("pure selection refusal changed persistent charge", err)
				}
				return
			}
			if name == "missing-pool-extent" {
				if !errors.Is(restoreErr, ErrPhysicalReceiptRequiresNoDebt) || s.physical != nil {
					t.Fatal("missing extent gained pending recovery permission", restoreErr)
				}
				if err := s.Close(); err != nil || o.Budget.Snapshot().Used.Disk != beforeClose {
					t.Fatal("early refusal changed the existing persistent obligation", err)
				}
				return
			}
			if name == "legacy-no-root-authority" {
				if !errors.Is(restoreErr, ErrPhysicalReceiptRequiresNoDebt) || s.physical != nil {
					t.Fatal("old receipt gained pending recovery permission", restoreErr)
				}
				if _, err := s.EnablePhysicalTerminalQualified(ctx, fixture.Selection, native, verify); err != nil {
					t.Fatal("existing NoDebt cleanup compatibility was lost", err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				for i := 0; i < 2; i++ {
					if err := s.Close(); err == nil || o.Budget.Snapshot().Used.Disk != beforeClose {
						t.Fatal("unknown target debt became healthy or refunded", err)
					}
				}
			}
		})
	}
}

func TestPhysicalRestoreActualEarliestSIGKILLRequiresNoDebtPreservation(t *testing.T) {
	requirePhysicalReceiptExt4(t)
	for _, phase := range []string{"physical-control-created-0", "physical-pool-created", "physical-pool-owned"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			creator, o := testStoreWithoutCleanup(t, 3)
			root, err := creator.RootIdentity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := creator.Close(); err != nil {
				t.Fatal(err)
			}
			nativePath := filepath.Join(filepath.Dir(creator.dir), "native")
			if err := os.WriteFile(nativePath, nil, 0600); err != nil {
				t.Fatal(err)
			}
			fixture := physicalReceiptCrashFixture{Directory: creator.dir, Native: nativePath, Root: root, Selection: nextCut(99)}
			raw, err := json.Marshal(fixture)
			if err != nil {
				t.Fatal(err)
			}
			fixturePath := filepath.Join(filepath.Dir(creator.dir), "fixture.json")
			if err := os.WriteFile(fixturePath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPhysicalReceiptSIGKILLHelper$", "-test.count=1")
			child.Env = append(os.Environ(), "HALRO_PHYSICAL_RECEIPT_FIXTURE="+fixturePath, "HALRO_PHYSICAL_RECEIPT_PHASE="+phase)
			output, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("not actual production earliest crash: %v %s", err, output)
			}
			trace := "terminal-root-00"
			if phase == "physical-control-created-0" {
				trace = "terminal.physical.0"
			}
			captured, err := os.Stat(filepath.Join(creator.dir, trace))
			if err != nil {
				t.Fatal(err)
			}
			o.ExpectedRoot = &root
			s, err := OpenStore(ctx, creator.dir, o)
			if err != nil {
				t.Fatal(err)
			}
			native, verify := receiptNativeFixture(t, nativePath)
			window, _, found, err := s.RestorePhysicalTerminalForRecovery(ctx, fixture.Selection, native, o.ExpectedCut, verify)
			if !errors.Is(err, ErrPhysicalReceiptRequiresNoDebt) || found || window != nil || s.physical != nil {
				t.Fatal("incomplete preparation gained borrow authority", found, err)
			}
			// This isolated fixture's external caller owns the NoDebt qualification.
			// The separate App test exercises the actual full-native/RecoveryView gate.
			if _, err := s.EnablePhysicalTerminalQualified(ctx, fixture.Selection, native, verify); err != nil {
				t.Fatal("NoDebt first preparation retry", err)
			}
			if phase != "physical-pool-owned" {
				preserved, err := os.Stat(filepath.Join(s.dir, "terminal-unqualified-0", trace))
				if err != nil || !os.SameFile(captured, preserved) {
					t.Fatal("unknown trace was deleted or replaced", err)
				}
			}
			fresh, err := s.ReservePhysicalTerminals(ctx, 2)
			if err != nil {
				t.Fatal(err)
			}
			if err := fresh.Close(); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
