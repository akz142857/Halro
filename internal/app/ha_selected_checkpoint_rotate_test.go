package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/store/lock"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Real TLS carries new facts every round, including rounds beyond the former
// four-file limit. Every publication is then opened by the normal independent
// Member/Ordering/source factory before another producer is started.
func TestHASelectedChangedCapsulesRecycleThroughActualTLSAndFreshReopen(t *testing.T) {
	exerciseHACacheCycles(t, "")
}

func TestHASelectedChangedCapsulesActualSIGKILLRecyclesIndependentSelectedSlot(t *testing.T) {
	for _, phase := range []string{"audit-pending", "audit-capsule", "governance-pending", "governance-capsule"} {
		t.Run(phase, func(t *testing.T) {
			stressPhase(t, phase, "audit-pending", "governance-capsule")
			exerciseHACacheCycles(t, phase)
		})
	}
}

func TestHASelectedChangedCapsulesRejectForeignAfterQualification(t *testing.T) {
	exerciseHACacheCycles(t, "foreign")
}
func TestHASelectedChangedCapsulesPendingExhaustionPreservesAllDebt(t *testing.T) {
	exerciseHACacheCycles(t, "exhausted")
}

func TestHASelectedChangedCapsulesUnknownActualReaderCloseRetainsSourceAndFees(t *testing.T) {
	exerciseHACacheCycles(t, "close")
}

func TestHASelectedChangedCapsulesRejectValidCacheMACWithWrongNativePrefix(t *testing.T) {
	exerciseHACacheCycles(t, "prefix")
}

func exerciseHACacheCycles(t *testing.T, killPhase string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture, owners, _ := prepareHASelectedTransportFixture(t, ctx)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for round := 1; round <= 8; round++ {
		runCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 2)
		for _, f := range owners {
			go func(f *haSelectedRuntimeFoundation) { done <- f.serveSelectedTransport(runCtx) }(f)
		}
		joined := false
		join := func() {
			stop()
			if !joined {
				for range 2 {
					if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
						t.Error(err)
					}
				}
				joined = true
			}
		}
		t.Cleanup(join)
		for !owners[0].replication.manager.PeerConnections()[fixture.configs[1].Replication.NodeID] {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
		c := owners[0].reopened.Owners.Components
		now := time.Now().UTC()
		a, err := c.Audit.Append(ctx, audit.Event{EventID: fmt.Sprintf("recycle-audit-%d", round), OccurredAt: now, ActorType: "admin", ActorID: "admin_actual", Action: "key.create", TargetType: "key", TargetID: "key_actualha", Outcome: "success"})
		if err != nil {
			t.Fatal(round, "actual Audit", err)
		}
		event := governance.Event{EventID: fmt.Sprintf("recycle-gov-%d", round), ProjectID: "prj_actualha", WorkUnitID: "wku_actualha", DefinitionID: "odef_result", DefinitionVersion: 1, OutcomeID: fmt.Sprintf("out_recycle%d", round), Value: "accepted", ReporterKeyID: "key_actualha", ObservedAt: now, IngestedAt: now, Revision: uint64(round), IdempotencyKeyHash: fmt.Sprintf("sha256:%064x", round), RequestFingerprint: fmt.Sprintf("sha256:%064x", round+100)}
		if round > 1 {
			event.SupersedesOutcomeID = fmt.Sprintf("out_recycle%d", round-1)
		}
		g, err := c.NativeGovernance.Append(ctx, event)
		if err != nil {
			t.Fatal(round, "actual Governance", err)
		}
		if _, err := c.Governance.AdvanceAuthenticatedFromNative(ctx, c.NativeGovernance, governance.Summary{Records: g.Sequence, Bytes: g.Offset, LastHash: g.Hash}, c.Budget); err != nil {
			t.Fatal(err)
		}
		index := owners[0].replication.publisher.Snapshot().DurableIndex
		if err := owners[0].replication.coordinator.WaitConfirmed(ctx, index); err != nil {
			t.Fatal(err)
		}
		for owners[1].replication.publisher.Snapshot().AppliedIndex < index {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
		join()
		for i, f := range owners {
			guard := f.owners.Components.Guard
			if err := f.Close(); err != nil {
				t.Fatal(round, "join old native", i, err)
			}
			var retained map[string][]byte
			if killPhase != "" && killPhase != "foreign" && killPhase != "exhausted" && killPhase != "close" && killPhase != "prefix" && round == 5 {
				active := map[string][]byte{}
				for _, p := range []string{f.auditCachePath, f.governanceCachePath} {
					raw, err := os.ReadFile(p)
					if err != nil {
						t.Fatal(err)
					}
					active[p] = raw
				}
				if err := guard.Close(); err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				spec := haSelectedCheckpointCrashSpec{Config: fixture.configs[i], Keys: fixture.keys, Phase: killPhase, Marker: filepath.Join(dir, "actual-phase")}
				raw, err := json.Marshal(spec)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "spec.json")
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				clear(raw)
				binary, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(binary, "-test.run", "^TestHASelectedCheckpointSIGKILLChild$", "-test.timeout=40s")
				cmd.Env = append(os.Environ(), "HALRO_HA_SELECTED_CHECKPOINT_CHILD="+path)
				output, err := cmd.CombinedOutput()
				var exited *exec.ExitError
				if !errors.As(err, &exited) {
					t.Fatalf("actual child not killed: %v %s", err, output)
				}
				status, ok := exited.Sys().(syscall.WaitStatus)
				if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
					t.Fatalf("wrong child termination: %v %s", err, output)
				}
				marker, err := os.ReadFile(spec.Marker)
				if err != nil || string(marker) != killPhase {
					t.Fatal("missing actual kill phase", err)
				}
				for p, want := range active {
					got, err := os.ReadFile(p)
					if err != nil || string(got) != string(want) {
						t.Fatal("selected capsule replaced before common", p, err)
					}
				}
				retained = map[string][]byte{}
				for n := range haSelectedCacheSlots {
					for _, gov := range []bool{false, true} {
						p := haSelectedCachePath(fixture.configs[i].Storage.DataDir, n, gov) + ".pending"
						raw, err := os.ReadFile(p)
						if err == nil {
							retained[p] = raw
						} else if !errors.Is(err, os.ErrNotExist) {
							t.Fatal(err)
						}
					}
				}
				guard, err = lock.Acquire(fixture.configs[i].Storage.DataDir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := guard.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			if (killPhase == "foreign" || killPhase == "exhausted" || killPhase == "close" || killPhase == "prefix") && round == 5 {
				var target, backup string
				var original []byte
				oldPending := map[string][]byte{}
				testCtx := ctx
				if killPhase == "foreign" {
					target = haSelectedCachePath(fixture.configs[i].Storage.DataDir, 1, false)
					backup = filepath.Join(t.TempDir(), "original-inactive")
					testCtx = context.WithValue(ctx, haSelectedCheckpointPhaseKey{}, func(phase string) {
						if phase != "audit-pending" {
							return
						}
						if err := os.Rename(target, backup); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(target, []byte("foreign creator preserved"), 0600); err != nil {
							t.Fatal(err)
						}
					})
				} else if killPhase == "prefix" {
					target = haSelectedCachePath(fixture.configs[i].Storage.DataDir, 1, false)
					var err error
					original, err = os.ReadFile(target)
					if err != nil {
						t.Fatal(err)
					}
					changed := bytes.Clone(original)
					changed[56] ^= 1
					mac := hmac.New(sha256.New, fixture.keys.Audit)
					_, _ = mac.Write([]byte("halro:audit:startup-retry-cache:v1\x00"))
					_, _ = mac.Write(changed[:len(changed)-sha256.Size])
					copy(changed[len(changed)-sha256.Size:], mac.Sum(nil))
					if err := os.WriteFile(target, changed, 0600); err != nil {
						t.Fatal(err)
					}
					// Establish that this is a valid cache MAC, rather than a parser rejection.
					m := functionalHAMetadataManager(t, fixture.configs[i])
					cache, err := audit.ReadStartupCache(ctx, bytes.NewReader(changed), int64(len(changed)), fixture.keys.Audit, f.bootstrap.Evidence.Identity, m)
					if err != nil {
						t.Fatal("invalid wrong-prefix fixture", err)
					}
					if err := cache.Close(); err != nil {
						t.Fatal(err)
					}
				} else if killPhase == "close" {
					testCtx = context.WithValue(ctx, haCheckpointCacheCloseKey{}, func(file *os.File) error {
						if file.Name() == haSelectedCachePath(fixture.configs[i].Storage.DataDir, 1, false) {
							return errors.New("unknown actual inactive reader close")
						}
						return file.Close()
					})
				} else {
					for n := range haSelectedCacheSlots {
						for _, gov := range []bool{false, true} {
							p := haSelectedCachePath(fixture.configs[i].Storage.DataDir, n, gov) + ".pending"
							raw := []byte(fmt.Sprintf("opaque-%d-%t", n, gov))
							if err := os.WriteFile(p, raw, 0600); err != nil {
								t.Fatal(err)
							}
							oldPending[p] = raw
						}
					}
				}
				manager := functionalHAMetadataManager(t, fixture.configs[i])
				failed, err := CheckpointHASelectedRuntime(testCtx, fixture.configs[i], fixture.keys.Master, guard, manager, logger)
				if killPhase == "close" {
					var debt *haCheckpointCacheInspectionFailure
					if failed == nil || !errors.Is(err, ownedfile.ErrRetirement) || !errors.As(err, &debt) || debt.files[0] == nil || failed.Publication.Seal.RootCount != 0 {
						t.Fatal("actual reader uncertainty escaped", err)
					}
					cost, e := debt.work.Cost()
					if e != nil || cost != (resourcebudget.Cost{RAM: 64 << 10}) {
						t.Fatal("reader IO fee lost", cost, e)
					}
					used := manager.Snapshot().Used
					if !errors.Is(failed.Close(), ownedfile.ErrRetirement) || !errors.Is(failed.Close(), ownedfile.ErrRetirement) || manager.Snapshot().Used != used || !bytes.Equal(failed.owner.bootstrap.AuditKey, fixture.keys.Audit) {
						t.Fatal("unknown reader Close refunded or cleared source keys")
					}
					if _, err := debt.files[0].Stat(); err != nil {
						t.Fatal("actual retained reader gone", err)
					}
					// Test-owned unknown seam is ended explicitly; production debt stays sticky.
					if err := debt.files[0].Close(); err != nil {
						t.Fatal(err)
					}
					return
				}
				if failed != nil {
					t.Cleanup(func() {
						if err := failed.Close(); err != nil {
							t.Error(err)
						}
					})
				}
				want := ownedfile.ErrReplaced
				if killPhase == "exhausted" {
					want = authenticatedstate.ErrLimit
				} else if killPhase == "prefix" {
					want = audit.ErrSealSource
				}
				if !errors.Is(err, want) || failed == nil || failed.Publication.Seal.RootCount != 0 {
					t.Fatal("unsafe capsule failure", killPhase, err)
				}
				if killPhase == "foreign" {
					got, err := os.ReadFile(target)
					if err != nil || string(got) != "foreign creator preserved" {
						t.Fatal("foreign inode changed", err)
					}
					if _, err := os.Lstat(target + ".pending"); err != nil {
						t.Fatal("actual candidate owner lost", err)
					}
					// Only the test's known creators restore this controlled fault.
					if err := failed.caches.Audit.Discard(); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(backup, target); err != nil {
						t.Fatal(err)
					}
				} else if killPhase == "prefix" {
					got, err := os.ReadFile(target)
					if err != nil || bytes.Equal(got, original) {
						t.Fatal("wrong-prefix capsule changed", err)
					}
					if _, err := os.Lstat(target + ".pending"); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("wrong-prefix minted candidate", err)
					}
					if err := os.WriteFile(target, original, 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					for p, want := range oldPending {
						got, err := os.ReadFile(p)
						if err != nil || string(got) != string(want) {
							t.Fatal("opaque pending overwritten", p, err)
						}
						if err := os.Remove(p); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			cp, err := CheckpointHASelectedRuntime(ctx, fixture.configs[i], fixture.keys.Master, guard, functionalHAMetadataManager(t, fixture.configs[i]), logger)
			if cp != nil {
				t.Cleanup(func() {
					if err := cp.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			if err != nil {
				t.Fatal(round, "true changed checkpoint", i, err)
			}
			seal := cp.Publication.Seal
			if seal.RootCount != 7 || seal.Revision != uint64(round+1) || seal.OrderingIndex != index || seal.Positions[3].Sequence != a.Sequence || seal.Positions[2].Sequence != g.Sequence {
				t.Fatal(round, "wrong cut", seal)
			}
			if err := cp.Close(); err != nil {
				t.Fatal(err)
			}
			fresh, err := openHASelectedRuntimeFoundation(ctx, fixture.configs[i], fixture.keys.Master, guard, functionalHAMetadataManager(t, fixture.configs[i]), logger)
			if fresh != nil {
				t.Cleanup(func() {
					if err := fresh.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			if err != nil {
				t.Fatal(round, "fresh independent cold factory", i, err)
			}
			if err := fresh.prepareSelectedServices(ctx); err != nil {
				t.Fatal(round, "actual cold seven roots", i, err)
			}
			got, found, err := fresh.reopened.Owners.Components.Governance.ReadOutcome(ctx, event.OutcomeID)
			if err != nil || !found || got.Value != event.Value {
				t.Fatal(round, "fresh lost new fact", i, got, found, err)
			}
			for p, want := range retained {
				got, err := os.ReadFile(p)
				if err != nil || string(got) != string(want) {
					t.Fatal("opaque pending creator guessed or replaced", p, err)
				}
			}
			if err := fresh.prepareSelectedTransport(logger); err != nil {
				t.Fatal(err)
			}
			owners[i] = fresh
			mains, pending := 0, 0
			for n := range haSelectedCacheSlots {
				for _, gov := range []bool{false, true} {
					p := haSelectedCachePath(fixture.configs[i].Storage.DataDir, n, gov)
					if _, e := os.Lstat(p); e == nil {
						mains++
					}
					if _, e := os.Lstat(p + ".pending"); e == nil {
						pending++
					}
				}
			}
			if mains > 2*haSelectedCacheSlots || pending > 1 || killPhase == "" && pending != 0 {
				t.Fatal("cache namespace grew", mains, pending)
			}
		}
	}
}
