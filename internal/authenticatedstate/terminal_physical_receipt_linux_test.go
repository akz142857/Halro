//go:build linux

package authenticatedstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

type physicalReceiptCrashFixture struct {
	Directory, Native string
	Root              RootIdentity
	Selection         [32]byte
}

func requirePhysicalReceiptExt4(t *testing.T) {
	t.Helper()
	var fs unix.Statfs_t
	if err := unix.Statfs(os.TempDir(), &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type != unix.EXT4_SUPER_MAGIC {
		t.Skip("requires dedicated ext4 physical receipt fixture")
	}
}

// This component fixture supplies its independently captured Store identity.
// It does not manufacture Runtime/HA qualification from a candidate header.
func receiptNativeFixture(t *testing.T, path string) (PhysicalNativeReference, func(PhysicalNativeReference, int64) error) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	id, err := resourcebudget.CapturePhysicalIdentity(f)
	if err != nil {
		t.Fatal(err)
	}
	parent, _, err := physicalPathIdentity(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	reference := PhysicalNativeReference{File: id, Parent: parent, Generation: 1}
	return reference, func(prior PhysicalNativeReference, end int64) error {
		if prior != reference || end != 0 {
			return ErrSealIneligible
		}
		return resourcebudget.CheckPhysicalTail(f, 0, end)
	}
}

func TestPhysicalReceiptSIGKILLHelper(t *testing.T) {
	configPath, phase := os.Getenv("HALRO_PHYSICAL_RECEIPT_FIXTURE"), os.Getenv("HALRO_PHYSICAL_RECEIPT_PHASE")
	if configPath == "" || phase == "" {
		t.Skip("subprocess helper")
	}
	var fixture physicalReceiptCrashFixture
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	o := storeOptions(t)
	o.ExpectedRoot = &fixture.Root
	o.DiskReservation, err = o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: o.MaxDiskBytes})
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(context.Background(), fixture.Directory, o)
	if err != nil {
		t.Fatal(err)
	}
	native, verify := receiptNativeFixture(t, fixture.Native)
	s.hook = func(reached string) error {
		if reached == phase {
			if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	if _, err := s.EnablePhysicalTerminalQualified(context.Background(), fixture.Selection, native, verify); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReservePhysicalTerminals(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	t.Fatal("actual SIGKILL phase not reached")
}

func TestPhysicalReceiptActualFirstPreparationSIGKILL(t *testing.T) {
	requirePhysicalReceiptExt4(t)
	for _, phase := range []string{"physical-control-created-0", "physical-control-allocated-0", "physical-control-initialized-0", "physical-control-created-1", "physical-control-allocated-1", "physical-control-initialized-1", "physical-genesis-synced", "physical-pool-created", "physical-pool-owned", "physical-pool-allocated", "foreign-inode", "extra-extent", "oversized-candidate", "opaque-slots-full", "oversized-opaque"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			creator, o := testStoreWithoutCleanup(t, 3)
			root, err := creator.RootIdentity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			pack, err := os.Stat(filepath.Join(creator.dir, "pages"))
			if err != nil {
				t.Fatal(err)
			}
			if err := creator.Close(); err != nil {
				t.Fatal(err)
			}
			nativePath := filepath.Join(filepath.Dir(creator.dir), "independently-owned-native")
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
			actualPhase := phase
			negative := phase == "foreign-inode" || phase == "extra-extent" || phase == "oversized-candidate" || phase == "opaque-slots-full" || phase == "oversized-opaque"
			if negative {
				actualPhase = "physical-pool-owned"
			}
			if phase == "opaque-slots-full" {
				actualPhase = "physical-pool-created"
			}
			child.Env = append(os.Environ(), "HALRO_PHYSICAL_RECEIPT_FIXTURE="+configPath, "HALRO_PHYSICAL_RECEIPT_PHASE="+actualPhase)
			output, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL || ctx.Err() != nil {
				t.Fatalf("not actual planned SIGKILL: %v %s", err, output)
			}
			spare := filepath.Join(creator.dir, "terminal-root-00")
			if phase == "foreign-inode" {
				if err := os.Rename(spare, spare+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(spare, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "extra-extent" || phase == "oversized-candidate" {
				path, offset, length := spare, int64(0), int64(8192)
				if phase == "extra-extent" {
					path = filepath.Join(creator.dir, "pages")
					offset = 1 << 30
					length = 4096
				}
				f, err := os.OpenFile(path, os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				allocateErr := unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_KEEP_SIZE, offset, length)
				if err := errors.Join(allocateErr, f.Sync(), f.Close()); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "opaque-slots-full" || phase == "oversized-opaque" {
				slots := 4
				if phase == "oversized-opaque" {
					slots = 1
				}
				for i := 0; i < slots; i++ {
					path := filepath.Join(creator.dir, fmt.Sprintf("terminal-unqualified-%d", i))
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
					if phase == "oversized-opaque" {
						if err := os.WriteFile(filepath.Join(path, "terminal.physical.0"), make([]byte, 8192), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			beforeSpare, err := os.Lstat(spare)
			if negative && err != nil {
				t.Fatal(err)
			}
			o.ExpectedRoot = &root
			s, err := OpenStore(ctx, creator.dir, o)
			if err != nil {
				t.Fatal(err)
			}
			native, verify := receiptNativeFixture(t, nativePath)
			_, enableErr := s.EnablePhysicalTerminalQualified(ctx, fixture.Selection, native, verify)
			if negative {
				if enableErr == nil {
					t.Fatal("foreign preparation granted ownership")
				}
				afterSpare, err := os.Lstat(spare)
				if err != nil || !os.SameFile(beforeSpare, afterSpare) || afterSpare.Size() != beforeSpare.Size() {
					t.Fatal("rejected preparation altered foreign spare", err)
				}
				beforeClose := o.Budget.Snapshot().Used
				closeErr := s.Close()
				if phase == "oversized-opaque" {
					// Rejection precedes acquiring a preparation owner or any mutation.
					if closeErr != nil {
						t.Fatal(closeErr)
					}
				} else if closeErr == nil || o.Budget.Snapshot().Used.Disk != beforeClose.Disk || s.physical == nil || s.physical.failure == nil {
					t.Fatal("unknown owner refunded capacity or returned healthy", closeErr)
				}
				return
			}
			if enableErr != nil {
				t.Fatal("actual bounded preparation recovery", enableErr)
			}
			after, err := s.RootIdentity(ctx)
			actualPack, statErr := os.Stat(filepath.Join(s.dir, "pages"))
			if err != nil || after != root || statErr != nil || !os.SameFile(pack, actualPack) {
				t.Fatal("candidate recovery changed independent source root", err, statErr)
			}
			if phase == "physical-pool-created" || phase[:16] == "physical-control" {
				if _, err := os.Stat(filepath.Join(s.dir, "terminal-unqualified-0")); err != nil {
					t.Fatal("unowned group was not preserved", err)
				}
			}
			window, err := s.ReservePhysicalTerminals(ctx, 2)
			if err != nil {
				t.Fatal("fresh real vector", err)
			}
			if err := window.Close(); err != nil {
				t.Fatal(err)
			}
			beforeClose := o.Budget.Snapshot().Used
			originalFee, err := s.physical.fee.Cost()
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal("actual joined receipt retirement", err)
			}
			opaque := phase == "physical-pool-created" || phase[:16] == "physical-control"
			fee, err := s.physical.fee.Cost()
			if opaque {
				if err != nil || fee.RAM != 0 || fee.Disk != originalFee.Disk {
					t.Fatal("shared capacity lost persistent unknown slots", fee, err)
				}
			} else if !s.physical.feeRetired || o.Budget.Snapshot().Used.Disk != beforeClose.Disk-originalFee.Disk {
				t.Fatal("known target retirement retained obsolete fee", o.Budget.Snapshot().Used, originalFee, err)
			}
			if err := s.RetirePhysicalPreparation(ctx); err != nil {
				t.Fatal("known repeated retirement", err)
			}
		})
	}
}
