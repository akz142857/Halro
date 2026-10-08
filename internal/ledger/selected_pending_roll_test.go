//go:build linux || darwin

package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

func selectedRollCrash(t *testing.T, f *selectedStartupFixture, phase string) {
	selectedMaintenanceCrash(t, f, "roll", phase)
}

func selectedMaintenanceCrash(t *testing.T, f *selectedStartupFixture, kind, phase string) {
	t.Helper()
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestLedgerMaintenanceSIGKILLHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "HALRO_LEDGER_MAINTENANCE_CRASH_PHASE="+phase, "HALRO_LEDGER_MAINTENANCE_CRASH_DIR="+filepath.Dir(f.path), "HALRO_LEDGER_MAINTENANCE_CRASH_KIND="+kind)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("child did not crash: %v %s", err, output)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL || ctx.Err() != nil {
		t.Fatalf("not intended SIGKILL: %v %s", err, output)
	}
	marker, err := os.ReadFile(filepath.Join(filepath.Dir(f.path), "crash-reached"))
	if err != nil || string(marker) != phase {
		t.Fatal("unproven crash boundary", string(marker), err)
	}
}

type selectedPendingCrashAuthority struct {
	NativePath, Directory string
	Identity              [32]byte
	MaxDisk, MaxAppend    int64
	Authority             authenticatedstate.SealEligibility
	Hard                  authenticatedstate.SealHardLimits
}

func TestSelectedPendingRecoverySIGKILLHelper(t *testing.T) {
	path := os.Getenv("HALRO_SELECTED_PENDING_AUTHORITY")
	if path == "" {
		t.Skip("subprocess-only selected recovery fixture")
	}
	raw, err := os.ReadFile(path)
	var a selectedPendingCrashAuthority
	if err != nil || json.Unmarshal(raw, &a) != nil {
		t.Fatal(err)
	}
	guard, err := lock.Acquire(filepath.Dir(a.NativePath))
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	budget := authenticatedLedgerTestOptions(t).Budget
	fee, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 32 << 10})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := authenticatedstate.OpenSealCandidatePublisher(filepath.Dir(a.NativePath), guard, testChainKey, a.Hard, budget, fee)
	if err != nil {
		t.Fatal(err)
	}
	phase := os.Getenv("HALRO_SELECTED_PENDING_PHASE")
	directoryFee, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: a.MaxDisk})
	if err != nil {
		t.Fatal(err)
	}
	o := SelectedSealOpenOptions{Publisher: pub, Guard: guard, Authority: a.Authority, Directory: a.Directory, DirectoryOptions: AuthenticatedDirectoryOptions{Identity: a.Identity, Budget: budget, MaxDiskBytes: a.MaxDisk, MaxAppendBytes: a.MaxAppend, DiskReservation: directoryFee}}
	o.recoveryPhase = func(at string) error {
		if at == phase {
			marker := filepath.Join(filepath.Dir(a.NativePath), "selected-recovery-crash-reached")
			f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := f.WriteString(at)
			if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	_, _, err = OpenWithSelectedSeal(context.Background(), a.NativePath, NewStatus(), Options{ChainKey: testChainKey}, o)
	t.Fatal("selected recovery crash boundary not reached", err)
}

func TestSelectedPendingRollRecoveryActualSIGKILLIsRestartable(t *testing.T) {
	for _, initial := range []string{"roll-intent-published", "roll-native-renamed", "roll-successor-created"} {
		phases := []string{"selected-roll-qualified", "selected-roll-manifest-published"}
		if initial != "roll-intent-published" {
			phases = append(phases, "selected-roll-successor-created", "selected-roll-successor-synced")
		}
		for _, phase := range phases {
			t.Run(initial+"/"+phase, func(t *testing.T) {
				f := newSelectedStartupFixture(t, true)
				if _, err := f.native.Append(context.Background(), projectionTestLifecycle(2, true)[0]); err != nil {
					t.Fatal(err)
				}
				want, hash, _ := f.native.ChainHead()
				selectedRollCrash(t, f, initial)
				seal, found, err := f.o.Publisher.Select(f.o.Authority)
				if err != nil || !found {
					t.Fatal(err)
				}
				hard := authenticatedstate.SealHardLimits{MaxPageBytes: seal.MaxPageBytes, MaxValueBytes: seal.MaxValueBytes, MaxTreeHeight: seal.MaxTreeHeight}
				for _, root := range seal.Roots[:seal.RootCount] {
					hard.RequiredRoots = append(hard.RequiredRoots, authenticatedstate.SealRootSchema{Kind: root.Kind, Component: root.Component, IndexID: root.IndexID, Codec: root.Codec})
				}
				a := selectedPendingCrashAuthority{NativePath: f.path, Directory: f.o.Directory, Identity: f.o.DirectoryOptions.Identity, MaxDisk: f.o.DirectoryOptions.MaxDiskBytes, MaxAppend: f.o.DirectoryOptions.MaxAppendBytes, Authority: f.o.Authority, Hard: hard}
				// This external fixture authority is supplied by the parent, never
				// derived from the child's pending manifest or candidate header.
				raw, err := json.Marshal(a)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "independent-authority.json")
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := f.o.Publisher.Close(); err != nil {
					t.Fatal(err)
				}
				credit, err := f.o.Publisher.DiskReservation()
				if err != nil {
					t.Fatal(err)
				}
				f.publisherCredit = credit
				if err := f.o.Guard.Close(); err != nil {
					t.Fatal(err)
				}
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, executable, "-test.run=^TestSelectedPendingRecoverySIGKILLHelper$", "-test.count=1")
				cmd.Env = append(os.Environ(), "HALRO_SELECTED_PENDING_AUTHORITY="+path, "HALRO_SELECTED_PENDING_PHASE="+phase)
				output, crashErr := cmd.CombinedOutput()
				guard, guardErr := lock.Acquire(filepath.Dir(f.path))
				if guardErr != nil {
					t.Fatal(guardErr)
				}
				f.o.Guard = guard
				pub, pubErr := authenticatedstate.OpenSealCandidatePublisher(filepath.Dir(f.path), guard, testChainKey, hard, f.o.DirectoryOptions.Budget, credit)
				if pubErr != nil {
					t.Fatal(pubErr)
				}
				f.o.Publisher = pub
				var exit *exec.ExitError
				if !errors.As(crashErr, &exit) {
					t.Fatalf("child did not crash: %v %s", crashErr, output)
				}
				status, ok := exit.Sys().(syscall.WaitStatus)
				if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL || ctx.Err() != nil {
					t.Fatalf("not intended SIGKILL: %v %s", crashErr, output)
				}
				marker, err := os.ReadFile(filepath.Join(filepath.Dir(f.path), "selected-recovery-crash-reached"))
				if err != nil || string(marker) != phase {
					t.Fatal("unproven selected recovery crash", string(marker), err)
				}
				r := f.reopen(t)
				if r.Head.Head.Sequence != want.Sequence || r.Head.Hash != hash || r.DecodedRecords != 1 {
					t.Fatal("restart duplicated or lost facts", r, want)
				}
				if err := f.native.AuditSelectedStartup(context.Background(), f.o.DirectoryOptions.Budget); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestSelectedPendingRollActualSIGKILLRecoversOnlyQualifiedFacts(t *testing.T) {
	for _, phase := range []string{"roll-native-synced", "roll-intent-published", "roll-before-rename", "roll-native-renamed", "roll-rename-synced", "roll-successor-created", "roll-successor-synced", "roll-manifest-published", "roll-index-published", "roll-before-previous-close", "roll-previous-closed"} {
		t.Run(phase, func(t *testing.T) {
			f := newSelectedStartupFixture(t, true)
			ctx := context.Background()
			if _, err := f.native.Append(ctx, projectionTestLifecycle(2, true)[0]); err != nil {
				t.Fatal(err)
			}
			want, wantHash, _ := f.native.ChainHead()
			selectedRollCrash(t, f, phase)
			r := f.reopen(t)
			if r.Head.Head.Sequence != want.Sequence || r.Head.Hash != wantHash || r.DecodedRecords != 1 {
				t.Fatal("qualified recovery lost or duplicated facts", r, want)
			}
			if err := f.native.AuditSelectedStartup(ctx, f.o.DirectoryOptions.Budget); err != nil {
				t.Fatal(err)
			}
			if !f.native.SelectedStartupReady() {
				t.Fatal("authenticated recovery not ready")
			}
			if _, err := f.native.Append(ctx, projectionTestLifecycle(3, true)[0]); err != nil {
				t.Fatal("recovered writer", err)
			}
			if _, err := f.native.Roll(); err != nil {
				t.Fatal("recovered Roll", err)
			}
			report, partial, err := InspectReplayAuthenticated(f.path, testChainKey, nil)
			if err != nil || partial || report.Head.Sequence != want.Sequence+1 {
				t.Fatal("recovered chain changed facts", report, partial, err)
			}
		})
	}
}

func TestSelectedPendingRollTamperRefusesBeforeNativeRepair(t *testing.T) {
	for _, phase := range []string{"roll-intent-published", "roll-native-renamed", "roll-successor-created"} {
		for _, damage := range []string{"old-history", "pending-history", "descriptor", "successor"} {
			if damage == "successor" && phase != "roll-successor-created" {
				continue
			}
			t.Run(phase+"/"+damage, func(t *testing.T) {
				f := newSelectedStartupFixture(t, true)
				if _, err := f.native.Append(context.Background(), projectionTestLifecycle(2, true)[0]); err != nil {
					t.Fatal(err)
				}
				selectedRollCrash(t, f, phase)
				directory := filepath.Dir(f.path)
				manifestPath := segmentManifestPath(directory)
				manifest, err := loadSegmentManifest(directory)
				if err != nil || manifest.Pending == nil {
					t.Fatal(err)
				}
				if damage == "descriptor" {
					manifest.Pending.FirstSequence++
					if err := saveSegmentManifest(directory, manifest); err != nil {
						t.Fatal(err)
					}
				} else {
					path := filepath.Join(directory, "ledger-1.wal")
					if damage == "pending-history" {
						path = filepath.Join(directory, manifest.Pending.File)
						if phase == "roll-intent-published" {
							path = f.path
						}
					} else if damage == "successor" {
						path = f.path
					}
					raw, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if len(raw) == 0 {
						raw = []byte("unauthorized successor")
					} else {
						raw[len(raw)-1] ^= 1
					}
					if err := os.WriteFile(path, raw, 0600); err != nil {
						t.Fatal(err)
					}
				}
				before, err := os.ReadFile(manifestPath)
				if err != nil {
					t.Fatal(err)
				}
				_, activeBefore := os.Lstat(f.path)
				native, _, err := OpenWithSelectedSeal(context.Background(), f.path, NewStatus(), Options{ChainKey: testChainKey}, f.o)
				if err == nil || native != nil {
					t.Fatal("damaged pending source accepted", err)
				}
				after, err := os.ReadFile(manifestPath)
				_, activeAfter := os.Lstat(f.path)
				if err != nil || !bytes.Equal(before, after) || errors.Is(activeBefore, os.ErrNotExist) != errors.Is(activeAfter, os.ErrNotExist) {
					t.Fatal("refusal mutated manifest or created missing successor", err)
				}
			})
		}
	}
}

func TestSelectedPendingRollWithdrawnSelectionOrCancelDoesNotCreateSuccessor(t *testing.T) {
	for _, mode := range []string{"selection", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := newSelectedStartupFixture(t, true)
			if _, err := f.native.Append(context.Background(), projectionTestLifecycle(2, true)[0]); err != nil {
				t.Fatal(err)
			}
			selectedRollCrash(t, f, "roll-native-renamed")
			manifestPath := segmentManifestPath(filepath.Dir(f.path))
			before, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			o := f.o
			o.recoveryPhase = func(phase string) error {
				if phase != "selected-roll-qualified" {
					return nil
				}
				if mode == "cancel" {
					cancel()
					return nil
				}
				seal, found, err := o.Publisher.Select(o.Authority)
				if err != nil || !found {
					return errors.Join(ErrProjectionSource, err)
				}
				seal.Revision++
				_, err = o.Publisher.Publish(context.Background(), seal)
				return err
			}
			native, _, err := OpenWithSelectedSeal(ctx, f.path, NewStatus(), Options{ChainKey: testChainKey}, o)
			if err == nil || native != nil || mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("withdrawn source repaired", err)
			}
			after, readErr := os.ReadFile(manifestPath)
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatal("withdrawal changed native manifest", readErr)
			}
			if _, err := os.Lstat(f.path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("withdrawal created successor", err)
			}
		})
	}
}
