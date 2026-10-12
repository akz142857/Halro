//go:build linux || darwin

package authenticatedstate

import (
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestStoreCOWCrashHelper(t *testing.T) {
	phase := os.Getenv("HALRO_AUTH_COW_CRASH_PHASE")
	if phase == "" {
		t.Skip("subprocess-only crash helper")
	}
	o := storeOptions(t)
	credit, e := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: o.MaxDiskBytes})
	if e != nil {
		t.Fatal(e)
	}
	o.DiskReservation = credit
	s, e := OpenStore(context.Background(), os.Getenv("HALRO_AUTH_COW_CRASH_DIR"), o)
	if e != nil {
		t.Fatal(e)
	}
	s.hook = func(p string) error {
		if p == phase {
			if e := os.WriteFile(filepath.Join(s.dir, "phase-reached"), []byte(p), 0o600); e != nil {
				t.Fatal(e)
			}
			if e := syscall.Kill(os.Getpid(), syscall.SIGKILL); e != nil {
				t.Fatal(e)
			}
			// kill may return before delivery; returning here would let the
			// next step (the rename after root-synced) run first.
			select {}
		}
		return nil
	}
	if _, e = s.Update(context.Background(), nextCut(1), func(tx *Transaction) error {
		if e := tx.Put([]byte("key_000001"), []byte("new")); e != nil {
			return e
		}
		return tx.Delete([]byte("key_000002"))
	}); e != nil {
		t.Fatal(e)
	}
	t.Fatal("crash hook was not reached")
}
func TestStoreActualSIGKILLRetainsWholeEligibleRoot(t *testing.T) {
	phases := []string{"pages-written", "pages-synced", "root-written", "root-synced", "root-renamed", "directory-synced"}
	for index, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			s, o := testStore(t, 20)
			if e := s.Close(); e != nil {
				t.Fatal(e)
			}
			executable, e := os.Executable()
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestStoreCOWCrashHelper$", "-test.count=1")
			cmd.Env = append(os.Environ(), "HALRO_AUTH_COW_CRASH_PHASE="+phase, "HALRO_AUTH_COW_CRASH_DIR="+s.dir)
			output, e := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(e, &exit) {
				t.Fatalf("child did not terminate at crash: %v %s", e, output)
			}
			status, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL || ctx.Err() != nil {
				t.Fatalf("not the planned SIGKILL: %v %s", e, output)
			}
			reached, e := os.ReadFile(filepath.Join(s.dir, "phase-reached"))
			if e != nil || string(reached) != phase {
				t.Fatal("unproven crash phase", string(reached), e)
			}
			if e = os.Remove(filepath.Join(s.dir, "phase-reached")); e != nil {
				t.Fatal(e)
			}
			if index >= 4 {
				o.ExpectedCut = nextCut(1)
			}
			reopened, e := OpenStore(context.Background(), s.dir, o)
			if e != nil {
				t.Fatal(e)
			}
			rows := storeRows(t, reopened)
			if index < 4 {
				if len(rows) != 20 || string(rows[1].Value) != "value_000001" {
					t.Fatal("partial old root")
				}
			} else {
				if len(rows) != 19 || string(rows[1].Value) != "new" || string(rows[2].Key) != "key_000003" {
					t.Fatal("partial new root")
				}
			}
			if e = reopened.Close(); e != nil {
				t.Fatal(e)
			}
			// The original namespace publisher adopts a fully closed, validated
			// successor through the actual receipt API, never a private-field edit.
			if e = s.AdoptClosedSuccessor(reopened); e != nil {
				t.Fatal(e)
			}
		})
	}
}
