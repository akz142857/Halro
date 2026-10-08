package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/usage"
)

// Real native MAC frames, external Governance anchors, private disk candidates
// and borrowed Ledger leases. Primary fixtures exercise construction authority;
// the existing real TLS pair tests supply confirmation/readiness evidence.
func projectionWorkspaceFixture(t *testing.T, kind string) (string, func(context.Context, *resourcePressureRuntime) (func() error, error), func()) {
	t.Helper()
	b, _ := recoveryResourceFixture(t)
	cfg := testConfig(t)
	cfg.ResourceBudget = b.ResourceBudget
	if err := os.MkdirAll(cfg.Storage.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	if kind == "usage_standalone" || kind == "usage_primary" || kind == "usage_primary_owned" {
		options := ledger.Options{}
		var member *replicationRuntime
		if kind != "usage_standalone" {
			member = primaryCompletionFixture(t)
			state := member.publisher.Snapshot()
			cfg.Replication = &config.Replication{ClusterID: state.ClusterID, NodeID: state.NodeID}
			options.AfterDurable = func(batch ledger.DurableBatch) (uint64, error) { return batch.LastSequence, nil }
			options.WaitConfirmed = func(context.Context, uint64) error { return nil }
		}
		native, path, key := projectedWriterLog(t, []ledger.Event{projectedWriterAcceptance("workspace")}, options)
		var lease *ledger.MaintenanceLease
		var projection *ledger.LedgerProjection
		if kind != "usage_primary_owned" {
			lease, projection = projectedWriterCandidate(t, native, path, key)
		}
		checkBorrowed := func() {
			t.Helper()
			if kind == "usage_primary_owned" {
				owned, err := native.BeginMaintenance(context.Background())
				if err != nil {
					t.Fatal("Usage retained its private native lease", err)
				}
				if err := owned.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(standaloneUsageProjectionPath(cfg)); errors.Is(err, os.ErrNotExist) {
					if _, err := os.Stat(primaryUsageLedgerPath(cfg)); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("private Ledger candidate leaked", err)
					}
				}
				return
			}
			if _, _, err := projection.ReadPosition(context.Background()); err != nil {
				t.Fatal("Usage retired the borrowed Ledger", err)
			}
			// WithHead failure may latch recovery-required. Maintenance itself
			// must stay borrowed, including on panic; mutation remains excluded.
			if _, err := native.BeginMaintenance(context.Background()); err == nil {
				t.Fatal("Usage released the borrowed native lease")
			}
		}
		return standaloneUsageProjectionPath(cfg), func(ctx context.Context, pressure *resourcePressureRuntime) (func() error, error) {
			var view *runtimeUsage
			var err error
			if member == nil {
				view, _, err = prepareStandaloneUsage(ctx, cfg, native, projection, lease, pressure)
			} else {
				view, _, err = preparePrimaryUsageOwner(ctx, cfg, native, member, projection, lease, pressure)
			}
			if err != nil {
				if view != nil {
					t.Fatal("failed construction published Usage")
				}
				return nil, err
			}
			if _, err := view.disk.ReadReplayAnchor(context.Background()); err != nil {
				t.Fatal(err)
			}
			return view.closeOwner, nil
		}, checkBorrowed
	}
	store, log, source, key := openGovernanceRecoveryFixture(t)
	t.Cleanup(func() { log.Close(); store.Close() })
	if err := os.MkdirAll(filepath.Dir(cfg.GovernancePath()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, cfg.GovernancePath()); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(context.Background(), governanceRecoveryEvent("out_workspace", "accepted", "", "workspace", 1)); err != nil {
		t.Fatal(err)
	}
	head, auth := log.AuthenticatedHead()
	if err := store.PutGovernanceJournalAnchor(head.Records, head.Bytes, head.LastHash, auth); err != nil {
		t.Fatal(err)
	}
	var member *replicationRuntime
	if kind == "governance_primary" {
		member = primaryCompletionFixture(t)
		state := member.publisher.Snapshot()
		cfg.Replication = &config.Replication{ClusterID: state.ClusterID, NodeID: state.NodeID}
	}
	return standaloneGovernanceProjectionPath(cfg), func(ctx context.Context, pressure *resourcePressureRuntime) (func() error, error) {
		owner, err := prepareRuntimeGovernance(ctx, cfg, store, log, key, member, pressure)
		if err != nil {
			if owner != nil {
				t.Fatal("failed construction published Governance")
			}
			return nil, err
		}
		actual, err := owner.projection.ReadHead(context.Background())
		if err != nil || actual != head {
			t.Fatal(actual, head, err)
		}
		return owner.close, nil
	}, func() {}
}

func TestProjectionWorkspaceRefusalPreservesSavedUsageAndGovernance(t *testing.T) {
	for _, kind := range []string{"usage_standalone", "usage_primary", "usage_primary_owned", "governance_standalone", "governance_primary"} {
		for _, fault := range []string{"missing_manager", "exhausted", "closed", "cancelled"} {
			t.Run(kind+"/"+fault, func(t *testing.T) {
				_, pressure := recoveryResourceFixture(t)
				manager := pressure.logicalBudget()
				path, run, checkBorrowed := projectionWorkspaceFixture(t, kind)
				old := []byte("untrusted saved candidate")
				if err := os.WriteFile(path, old, 0600); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				switch fault {
				case "missing_manager":
					pressure = nil
				case "exhausted":
					permit, err := manager.TryAcquireRecovery(resourcebudget.Cost{RAM: runtimeRAMBudgetFixture().RecoveryRAMBytes})
					if err != nil {
						t.Fatal(err)
					}
					defer permit.Release()
				case "closed":
					manager.CloseAdmission()
				case "cancelled":
					cancel()
				}
				before := manager.Snapshot().Used
				if cleanup, err := run(ctx, pressure); err == nil || cleanup != nil {
					t.Fatal("refused constructor published a candidate", err)
				}
				got, err := os.ReadFile(path)
				if err != nil || string(got) != string(old) {
					t.Fatal("quota refusal mutated saved derivative", err)
				}
				checkBorrowed()
				if manager.Snapshot().Used != before {
					t.Fatal("refused constructor leaked credit", manager.Snapshot())
				}
			})
		}
	}
}

func TestProjectionWorkspaceCancellationAndPanicCleanCandidatesBeforeRetirement(t *testing.T) {
	for _, kind := range []string{"usage_standalone", "usage_primary", "usage_primary_owned", "governance_standalone", "governance_primary"} {
		for _, fault := range []string{"cancel", "panic"} {
			t.Run(kind+"/"+fault, func(t *testing.T) {
				b, pressure := recoveryResourceFixture(t)
				manager := pressure.logicalBudget()
				path, run, checkBorrowed := projectionWorkspaceFixture(t, kind)
				base, cancel := context.WithCancel(context.Background())
				defer cancel()
				entered, resume := make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(resume) }) }
				defer unblock()
				var triggered atomic.Bool
				probe := generationProbeContext{Context: base, probe: func() {
					if _, err := os.Stat(path); err != nil || !triggered.CompareAndSwap(false, true) {
						return
					}
					close(entered)
					<-resume // candidate's actual construction stack is still live
					if fault == "panic" {
						panic("projection construction probe")
					}
				}}
				done := make(chan error, 1)
				go func() {
					defer func() {
						if got := recover(); got != nil {
							if got == "projection construction probe" {
								done <- nil
							} else {
								done <- errors.New("unexpected panic")
							}
						}
					}()
					cleanup, err := run(probe, pressure)
					if cleanup != nil {
						_ = cleanup()
						err = errors.New("interrupted constructor returned a usable candidate")
					}
					done <- err
				}()
				select {
				case <-entered:
				case err := <-done:
					t.Fatal("candidate boundary not reached", err)
				case <-time.After(3 * time.Second):
					t.Fatal("candidate boundary timed out")
				}
				if fault == "cancel" {
					cancel()
				}
				select {
				case err := <-done:
					t.Fatal("construction returned before candidate stack joined", err)
				case <-time.After(10 * time.Millisecond):
				}
				if s := manager.Snapshot(); s.RecoveryClaims != 1 || s.Classes[resourcebudget.Recovery].RAM != b.ResourceBudget.AccountingRecoveryRAMBytes {
					t.Fatal("live construction lost credit", s)
				}
				unblock()
				err := <-done
				if fault == "cancel" && !errors.Is(err, context.Canceled) || fault == "panic" && err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unpublished candidate leaked", err)
				}
				checkBorrowed()
				if s := manager.Snapshot(); s.Used != (resourcebudget.Cost{}) || s.Owners != 0 {
					t.Fatal("joined constructor leaked credit", s)
				}
			})
		}
	}
}

func TestProjectionWorkspaceSuccessfulConstructionRetiresOnlyTransientCredit(t *testing.T) {
	for _, kind := range []string{"usage_standalone", "usage_primary", "usage_primary_owned", "governance_standalone", "governance_primary"} {
		t.Run(kind, func(t *testing.T) {
			_, pressure := recoveryResourceFixture(t)
			path, run, checkBorrowed := projectionWorkspaceFixture(t, kind)
			cleanup, err := run(context.Background(), pressure)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if _, err := os.Stat(path); err != nil {
				t.Fatal("live projection disappeared", err)
			}
			checkBorrowed()
			if s := pressure.logicalBudget().Snapshot(); s.Used != (resourcebudget.Cost{}) || s.Acquired[resourcebudget.Recovery] != 1 {
				t.Fatal("live DB retained temporary credit", s)
			}
		})
	}
}

func TestProjectionWorkspaceUsageWindowRefusalAndFreshRetry(t *testing.T) {
	r, _ := summaryRuntime(t, 0)
	ctx := context.Background()
	now := time.Now().UTC()
	seedRetentionUsage(t, r, 1, now.AddDate(0, 0, -60))
	p := installDiskUsageQueries(t, r)
	e := bindStartupUsageArchive(t, r, usage.FormatNDJSON)
	if _, err := e.ExportDiskStep(ctx, p, usage.DefaultDiskExportStepLimits()); err != nil {
		t.Fatal(err)
	}
	before, err := p.ReadStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	archive := doctorTree(t, r.config.UsagePath())
	b, pressure := recoveryResourceFixture(t)
	r.config.ResourceBudget, r.resourcePressure = b.ResourceBudget, pressure
	// This test attaches only the job budget to already prepared native owners;
	// default full Open coverage below exercises the actual lifetime owners.
	t.Cleanup(func() { r.config.ResourceBudget, r.resourcePressure = nil, nil })
	lease, err := r.ledger.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	manager := pressure.logicalBudget()
	held, err := manager.TryAcquireRecovery(resourcebudget.Cost{RAM: b.ResourceBudget.RecoveryRAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.restoreUsageDiskWindow(ctx, lease, now)
	if err == nil || out != (usageStartupWindowReport{}) {
		t.Fatal("budget refusal returned usable window", out, err)
	}
	after, err := p.ReadStats(ctx)
	if err != nil || before != after {
		t.Fatal("quota refusal mutated query history", before, after, err)
	}
	actual := doctorTree(t, r.config.UsagePath())
	if len(actual) != len(archive) {
		t.Fatal("quota refusal mutated archive")
	}
	for path, hash := range archive {
		if actual[path] != hash {
			t.Fatal("quota refusal mutated archive", path)
		}
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	out, err = r.restoreUsageDiskWindow(ctx, lease, now)
	if err != nil || !out.Verified || out.Window.Attempts != 0 {
		t.Fatal("fresh retry failed to verify/prune archived rows", out, err)
	}
	if s := manager.Snapshot(); s.Used != (resourcebudget.Cost{}) || s.Acquired[resourcebudget.Recovery] != 2 {
		t.Fatal("window job retained transient credit", s)
	}
}
