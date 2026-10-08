//go:build linux || darwin

package authenticatedstate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/store/lock"
)

func TestSealCandidateCrashHelper(t *testing.T) {
	phase := os.Getenv("HALRO_SEAL_CRASH_PHASE")
	if phase == "" {
		t.Skip("subprocess-only SIGKILL fixture")
	}
	dir := os.Getenv("HALRO_SEAL_CRASH_DIR")
	key := make([]byte, 32)
	key[0] = 14
	budget := testBudget(t)
	reservation := sealPublisherReservation(t, budget)
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := sealCandidateFixture()
	p, err := openSealCandidatePublisher(dir, guard, key, sealHardFor(want), budget, reservation, func(at string) error {
		if at == phase {
			if err := os.WriteFile(filepath.Join(dir, "phase-reached"), []byte(at), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("HALRO_SEAL_CRASH_REPLACEMENT") == "1" {
		want.Revision++
	}
	if _, err := p.Publish(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	t.Fatal("crash phase was not reached")
}

func TestSealCandidateActualSIGKILLRecoversCommittedEnvelope(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		phases := []string{"seal-file-synced", "seal-renamed", "seal-directory-synced"}
		if !replacement {
			phases = append(phases, "seal-linked")
		}
		for _, phase := range phases {
			name := "initial/" + phase
			if replacement {
				name = "replacement/" + phase
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				key := make([]byte, 32)
				key[0] = 14
				want := sealCandidateFixture()
				hard := sealHardFor(want)
				if replacement {
					budget := testBudget(t)
					credit := sealPublisherReservation(t, budget)
					guard, err := lock.Acquire(dir)
					if err != nil {
						t.Fatal(err)
					}
					p, err := OpenSealCandidatePublisher(dir, guard, key, hard, budget, credit)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := p.Publish(context.Background(), want); err != nil {
						t.Fatal(err)
					}
					if err := p.Close(); err != nil {
						t.Fatal(err)
					}
					persistent, err := p.DiskReservation()
					if err != nil {
						t.Fatal(err)
					}
					// The seed process's accounting ends here; the next process
					// independently charges the namespace before any file reads.
					if err := persistent.Release(); err != nil {
						t.Fatal(err)
					}
					if err := credit.Release(); err != nil {
						t.Fatal(err)
					}
					if err := guard.Close(); err != nil {
						t.Fatal(err)
					}
				}
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, executable, "-test.run=^TestSealCandidateCrashHelper$", "-test.count=1")
				cmd.Env = append(os.Environ(), "HALRO_SEAL_CRASH_PHASE="+phase, "HALRO_SEAL_CRASH_DIR="+dir)
				if replacement {
					cmd.Env = append(cmd.Env, "HALRO_SEAL_CRASH_REPLACEMENT=1")
				}
				output, err := cmd.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("child did not crash: %v %s", err, output)
				}
				status, ok := exit.Sys().(syscall.WaitStatus)
				if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL || ctx.Err() != nil {
					t.Fatalf("not intended SIGKILL: %v %s", err, output)
				}
				reached, err := os.ReadFile(filepath.Join(dir, "phase-reached"))
				if err != nil || string(reached) != phase {
					t.Fatalf("unproven crash phase: %s %v", reached, err)
				}
				budget := testBudget(t)
				credit := sealPublisherReservation(t, budget)
				defer credit.Release()
				guard, err := lock.Acquire(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer guard.Close()
				p, err := OpenSealCandidatePublisher(dir, guard, key, hard, budget, credit)
				if err != nil {
					t.Fatal(err)
				}
				got, found, err := p.Current()
				wantFound := replacement || phase != "seal-file-synced"
				if replacement && phase != "seal-file-synced" {
					want.Revision++
				}
				if err != nil || found != wantFound || found && got != want {
					t.Fatalf("partial/pending envelope selected: %v %t %v", got, found, err)
				}
				if _, err := os.Lstat(filepath.Join(dir, "active.seal.pending")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unreconciled pending file: %v", err)
				}
				if err := p.Close(); err != nil {
					t.Fatal(err)
				}
				if err := p.Retire(); err != nil {
					t.Fatal(err)
				}
				if err := credit.Release(); err != nil {
					t.Fatal(err)
				}
				if used := budget.Snapshot().Used; used.RAM != 0 || used.Disk != 0 {
					t.Fatalf("joined owners left fees: %#v", used)
				}
			})
		}
	}
}
