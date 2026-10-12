//go:build linux || darwin

package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestLedgerMaintenanceSIGKILLHelper(t *testing.T) {
	phase := os.Getenv("HALRO_LEDGER_MAINTENANCE_CRASH_PHASE")
	if phase == "" {
		t.Skip("subprocess-only native maintenance SIGKILL fixture")
	}
	directory := os.Getenv("HALRO_LEDGER_MAINTENANCE_CRASH_DIR")
	log := openChained(t, filepath.Join(directory, "ledger.wal"), nil)
	log.maintenancePhase = func(at string) error {
		if at == phase {
			marker, err := os.OpenFile(filepath.Join(directory, "crash-reached"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := marker.WriteString(at)
			if err := errors.Join(writeErr, marker.Sync(), marker.Close()); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			// kill may return before delivery; park so no later step runs.
			select {}
		}
		return nil
	}
	var err error
	if os.Getenv("HALRO_LEDGER_MAINTENANCE_CRASH_KIND") == "compact" {
		_, err = log.Compact(1)
	} else {
		_, err = log.Roll()
	}
	t.Fatalf("crash boundary was not reached: %v", err)
}

func TestLedgerRollCompactActualSIGKILLPreservesAuthenticatedFacts(t *testing.T) {
	phases := map[string][]string{
		"roll":    {"roll-native-synced", "roll-intent-published", "roll-before-rename", "roll-native-renamed", "roll-rename-synced", "roll-successor-created", "roll-successor-synced", "roll-manifest-published", "roll-index-published", "roll-before-previous-close", "roll-previous-closed"},
		"compact": {"compact-artifact-published", "compact-manifest-published", "compact-index-published", "compact-plain-retired"},
	}
	for kind, boundaries := range phases {
		for _, phase := range boundaries {
			t.Run(kind+"/"+phase, func(t *testing.T) {
				directory := t.TempDir()
				path := filepath.Join(directory, "ledger.wal")
				log := openChained(t, path, nil)
				appendReservations(t, log, 1, 4)
				_, wantHash, ok := log.ChainHead()
				if !ok {
					t.Fatal("seed is unauthenticated")
				}
				if kind == "compact" {
					if _, err := log.Roll(); err != nil {
						t.Fatal(err)
					}
				}
				if err := log.Close(); err != nil {
					t.Fatal(err)
				}
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, executable, "-test.run=^TestLedgerMaintenanceSIGKILLHelper$", "-test.count=1")
				cmd.Env = append(os.Environ(), "HALRO_LEDGER_MAINTENANCE_CRASH_PHASE="+phase, "HALRO_LEDGER_MAINTENANCE_CRASH_DIR="+directory, "HALRO_LEDGER_MAINTENANCE_CRASH_KIND="+kind)
				output, err := cmd.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("child did not crash: %v %s", err, output)
				}
				status, ok := exit.Sys().(syscall.WaitStatus)
				if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL || ctx.Err() != nil {
					t.Fatalf("not intended SIGKILL: %v %s", err, output)
				}
				marker, err := os.ReadFile(filepath.Join(directory, "crash-reached"))
				if err != nil || string(marker) != phase {
					t.Fatalf("unproven crash phase: %q %v", marker, err)
				}
				reopened := openChained(t, path, nil)
				defer reopened.Close()
				ids, head := replayedEventIDs(t, reopened, Watermark{})
				_, gotHash, ok := reopened.ChainHead()
				if !ok || head.Sequence != 4 || gotHash != wantHash || len(ids) != 4 {
					t.Fatalf("native facts changed: %v %#v %x", ids, head, gotHash)
				}
				for i, id := range ids {
					if id != fmt.Sprintf("evt_%d", i+1) {
						t.Fatal("missing or duplicate fact", ids)
					}
				}
				if kind == "compact" {
					if _, err := reopened.Compact(1); err != nil {
						t.Fatal("interrupted compaction cannot complete", err)
					}
					if _, err := os.Lstat(filepath.Join(directory, "ledger-1.wal")); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("authenticated duplicate remained after joined retry", err)
					}
				}
				appendReservations(t, reopened, 5, 1)
				if _, err := reopened.Roll(); err != nil {
					t.Fatal("recovered writer cannot roll", err)
				}
				ids, head = replayedEventIDs(t, reopened, Watermark{})
				if len(ids) != 5 || head.Sequence != 5 {
					t.Fatal("recovered writer changed facts", ids, head)
				}
			})
		}
	}
}
