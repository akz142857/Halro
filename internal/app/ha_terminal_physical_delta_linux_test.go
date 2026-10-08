//go:build linux

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
)

type haPhysicalDeltaCrashRequest struct {
	Configs []config.Config
	Keys    HAMetadataKeys
	Ready   string
}

// The actual Primary receives its original native/Ordering durability receipt,
// then SIGKILL interrupts local COW Apply. Only its Ledger vector is physical;
// this does not prove full-HA ENOSPC, business Ready, or terminal confirmation.
func TestHAPhysicalActualDurableLedgerDeltaSIGKILLRestoresOriginalTargets(t *testing.T) {
	if os.Getenv("HALRO_EXT4_ROOT") == "" {
		t.Skip("isolated ext4 runner required")
	}
	fixture := prepareHAMetadataMaintenanceFixture(t)
	prepareHAMetadataMaintenanceArtifacts(t, fixture)
	completeHAMemoryActivationBoundary(t, fixture)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	for _, cfg := range fixture.configs {
		g, e := lock.Acquire(cfg.Storage.DataDir)
		if e != nil {
			t.Fatal(e)
		}
		a, e := ActivateHAMemoryRuntime(ctx, cfg, fixture.keys, g, functionalHAMetadataManager(t, cfg), logger)
		if e != nil {
			t.Fatal(e)
		}
		if e = errors.Join(a.Close(), g.Close()); e != nil {
			t.Fatal(e)
		}
	}
	requestPath := filepath.Join(t.TempDir(), "request.json")
	ready := filepath.Join(t.TempDir(), "native-durable")
	for i := range fixture.configs {
		limits := *fixture.configs[i].ResourceBudget
		limits.TotalRAMBytes = 2 << 30
		limits.RecoveryRAMBytes, limits.RecoveryReserveRAMBytes = 896<<20, 896<<20
		// These limits follow real seven-root activation above; the selected
		// single-request envelope is distinct from aggregate class capacity.
		limits.MemoryStateRequired = true
		limits.CompletionRAMBytes = 1 << 30
		limits.LedgerProjectionDiskBytes = 56 << 20
		limits.LedgerSegmentIndexDiskBytes = 48 << 20
		limits.UsageProjectionDiskBytes = 128 << 20
		limits.LedgerAppendDiskBytes = 64 << 20
		fixture.configs[i].ResourceBudget = &limits
	}
	// The Replica is a genuinely separate process owner (the parent test).
	// Killing Primary must not simultaneously kill its acknowledgement peer
	// inside an unrelated Ordering-to-Member publication crash window.
	replicaCfg := fixture.configs[1]
	replicaGuard, e := lock.Acquire(replicaCfg.Storage.DataDir)
	if e != nil {
		t.Fatal(e)
	}
	defer replicaGuard.Close()
	replica, e := openHASelectedRuntimeFoundation(ctx, replicaCfg, fixture.keys.Master, replicaGuard, functionalHAMetadataManager(t, replicaCfg), logger)
	if e != nil {
		t.Fatal("independent actual Replica", e)
	}
	defer func() {
		if e := replica.Close(); e != nil {
			t.Error("actual Replica join", e)
		}
	}()
	if e = replica.prepareSelectedServices(ctx); e != nil {
		t.Fatal("actual Replica services", e)
	}
	if e = replica.prepareSelectedTransport(logger); e != nil {
		t.Fatal("actual Replica transport", e)
	}
	peerCtx, stopPeer := context.WithCancel(ctx)
	peerDone := make(chan error, 1)
	go func() { peerDone <- replica.serveSelectedTransport(peerCtx) }()
	defer func() {
		stopPeer()
		if e := <-peerDone; e != nil && !errors.Is(e, context.Canceled) {
			t.Error("actual peer transport join", e)
		}
	}()
	raw, e := json.Marshal(haPhysicalDeltaCrashRequest{Configs: fixture.configs[:], Keys: fixture.keys, Ready: ready})
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(requestPath, raw, 0600); e != nil {
		t.Fatal(e)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestHAPhysicalActualDurableLedgerDeltaSIGKILLHelper$", "-test.count=1")
	command.Env = append(os.Environ(), "HALRO_HA_PHYSICAL_DELTA_REQUEST="+requestPath)
	output := &bytes.Buffer{}
	command.Stdout, command.Stderr = output, output
	if e = command.Start(); e != nil {
		t.Fatal(e)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	joined := false
	t.Cleanup(func() {
		if !joined {
			command.Process.Kill()
			<-waited
		}
	})
	until := time.Now().Add(30 * time.Second)
	for {
		if _, e = os.Stat(ready); e == nil {
			break
		}
		select {
		case e = <-waited:
			joined = true
			t.Fatal("child before actual native durability", e, output.String())
		default:
		}
		if time.Now().After(until) {
			command.Process.Kill()
			<-waited
			joined = true
			t.Fatal("actual native durability marker", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e = command.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	e = <-waited
	joined = true
	var exited *exec.ExitError
	if !errors.As(e, &exited) {
		t.Fatal("child did not receive actual SIGKILL", e, output.String())
	}
	status, ok := exited.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal("unexpected child exit", e, output.String())
	}
	cfg := fixture.configs[0]
	limits := *cfg.ResourceBudget
	limits.PhysicalTerminalReserve = true
	cfg.ResourceBudget = &limits
	guard, e := lock.Acquire(cfg.Storage.DataDir)
	if e != nil {
		t.Fatal(e)
	}
	defer guard.Close()
	manager := functionalHAMetadataManager(t, cfg)
	f, e := openHASelectedRuntimeFoundation(ctx, cfg, fixture.keys.Master, guard, manager, logger)
	if e != nil {
		t.Fatal("fresh independently allowed actual Primary", e)
	}
	defer func() {
		if e := f.Close(); e != nil {
			t.Error("actual recovery owner Close", e)
		}
	}()
	if e = f.prepareSelectedRoots(ctx); e != nil {
		t.Fatal("original physical receipt delta replay", e)
	}
	c := f.reopened.Owners.Components
	ref, e := c.Ledger.SealReference(ctx)
	if e != nil || ref.Anchor.Head.Sequence != 1 || !c.Ledger.PhysicalStartupRecoveryPending() {
		t.Fatal("actual delta not recovered with original window", ref.Anchor, e)
	}
	if f.replication.startupReady.Load() || f.business != nil {
		t.Fatal("recovery manufactured Ready")
	}
	// Recover the genuinely durable, previously unconfirmed Ordering suffix
	// through the original TLS coordinator and Replica. No Member field is
	// constructed by this component fixture, and no Runtime Ready is exposed.
	componentLimits := *f.cfg.ResourceBudget
	componentLimits.PhysicalTerminalReserve = false
	f.cfg.ResourceBudget = &componentLimits // already restored the original Ledger vector
	if e = f.prepareSelectedServicesWithLease(ctx, true); e != nil {
		t.Fatal("recovered actual Primary startup services", e)
	}
	if e = f.prepareHASelectedPrimaryStartup(ctx); e != nil {
		t.Fatal("original Primary startup confirmation worker", e)
	}
	if e = f.prepareSelectedTransport(logger); e != nil {
		t.Fatal("actual recovered Primary transport", e)
	}
	runCtx, stop := context.WithCancel(ctx)
	transportDone := make(chan error, 1)
	go func() { transportDone <- f.serveSelectedTransport(runCtx) }()
	defer func() {
		stop()
		if err := <-transportDone; err != nil && !errors.Is(err, context.Canceled) {
			t.Error("actual Primary transport join", err)
		}
	}()
	select {
	case <-f.replication.startupProducers:
	case e = <-f.replication.fatal:
		t.Fatal("actual recovered Ordering confirmation", e, "actual Replica", replica.replication.publisher.Snapshot(), "apply", replica.applyErr)
	case e = <-replica.replication.fatal:
		t.Fatal("actual surviving Replica apply", e)
	case <-ctx.Done():
		t.Fatal("actual recovered Ordering confirmation timeout", ctx.Err())
	}
	member := f.replication.publisher.Snapshot()
	if member.AppliedIndex == 0 || member.AppliedIndex != member.ConfirmedIndex || member.ConfirmedIndex != member.DurableIndex {
		t.Fatal("original Member lacks actual confirmed/apply proof", member)
	}
	if e = f.business.close(ctx); e != nil {
		t.Fatal("actual recovered services join", e)
	}
	lease, e := c.NativeLedger.BeginMaintenance(ctx)
	if e != nil {
		t.Fatal("actual post-confirmation maintenance lease", e)
	}
	f.owners.Components.Lease, f.reopened.Owners.Components.Lease = lease, lease
	c.Lease = lease
	if e = c.Usage.BindHASelectedTypedMaintenance(ctx, c.NativeLedger, lease, c.Ledger, manager); e != nil {
		t.Fatal("actual recovered Usage maintenance", e)
	}
	base := f.reopened.Seal
	base.Revision++
	pub, e := statecut.PublishHASelectedMaintenanceCapturedStartupCaches(ctx, f.reopened.Owners, base, func(ctx context.Context) (statecut.StartupCacheCandidates, error) {
		var candidates statecut.StartupCacheCandidates
		ac, err := c.Audit.CaptureHAStartupCache(ctx, manager, f.bootstrap.Evidence.Identity, c.Audit.Summary())
		if err != nil {
			return candidates, err
		}
		defer ac.Close()
		gc, err := c.NativeGovernance.CaptureHAStartupCache(ctx, manager, f.bootstrap.Evidence.Identity, c.NativeGovernance.Summary())
		if err != nil {
			return candidates, err
		}
		defer gc.Close()
		candidates.Audit, err = ac.PersistNew(ctx, haSelectedCachePath(cfg.Storage.DataDir, 1, false), audit.StartupCachePersistOptions{Guard: guard, Budget: manager, DiskReservation: f.cacheDisks[1][0], Identity: f.bootstrap.Evidence.Identity, Head: ac.Head()})
		if err != nil {
			return candidates, err
		}
		candidates.Governance, err = gc.PersistNew(ctx, haSelectedCachePath(cfg.Storage.DataDir, 1, true), governance.StartupCachePersistOptions{Guard: guard, Budget: manager, DiskReservation: f.cacheDisks[1][1], Identity: f.bootstrap.Evidence.Identity, Head: gc.Head()})
		return candidates, err
	})
	if e != nil {
		t.Fatal("actual all-root successor after delta", e)
	}
	if pub.Seal.RootCount != 7 || pub.Seal.Positions[0].Sequence != 1 {
		t.Fatal("incomplete recovered common", pub.Seal)
	}
	if e = c.Ledger.FinishPhysicalStartupRecovery(ctx, c.NativeLedger); e != nil {
		t.Fatal("original recovered work join", e)
	}
	if e = c.NativeLedger.RetireHAPhysicalPreparationAfterCommon(ctx, c.Lease); e != nil {
		t.Fatal("qualified actual native retirement", e)
	}
	raw, e = json.Marshal(pub.Seal)
	if e != nil {
		t.Fatal(e)
	}
	if e = c.NativeLedger.BindHAPhysicalSelectionDigestContext(ctx, c.Lease, sha256.Sum256(raw)); e != nil {
		t.Fatal("actual recovered successor binding", e)
	}
	if e = c.NativeLedger.CloseHAQualifiedPhysicalContext(ctx, c.Lease); e != nil {
		t.Fatal("actual recovered native Close", e)
	}
	if e = f.Close(); e != nil {
		t.Fatal("actual recovered foundation join", e)
	}
}

func TestHAPhysicalActualDurableLedgerDeltaSIGKILLHelper(t *testing.T) {
	path := os.Getenv("HALRO_HA_PHYSICAL_DELTA_REQUEST")
	if path == "" {
		t.Skip("actual subprocess only")
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var request haPhysicalDeltaCrashRequest
	if e = json.Unmarshal(raw, &request); e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var owners [2]*haSelectedRuntimeFoundation
	for i, cfg := range request.Configs[:1] {
		guard, e := lock.Acquire(cfg.Storage.DataDir)
		if e != nil {
			t.Fatal(e)
		}
		f, e := openHASelectedRuntimeFoundation(ctx, cfg, request.Keys.Master, guard, functionalHAMetadataManager(t, cfg), logger)
		if e != nil {
			t.Fatal(e)
		}
		owners[i] = f
		if e = f.prepareSelectedRoots(ctx); e != nil {
			t.Fatal(e)
		}
		if i == 0 {
			c := f.reopened.Owners.Components
			seal, found, e := c.Publisher.CurrentContext(ctx)
			if e != nil || !found {
				t.Fatal(e)
			}
			raw, e = json.Marshal(seal)
			if e != nil {
				t.Fatal(e)
			}
			if e = c.Ledger.EnableHAQualifiedPhysicalTerminals(ctx, c.NativeLedger, c.Lease, cfg.ResourceBudget.LedgerAppendDiskBytes, f.growth.credit, f.growth, sha256.Sum256(raw)); e != nil {
				t.Fatal(e)
			}
			// One real future slot survives the ordinary append's separately prepared
			// candidate; actual restore must consume only original retained targets.
			if _, e = c.NativeLedger.ReservePhysicalTerminals(ctx, 1); e != nil {
				t.Fatal(e)
			}
		}
		if e = f.prepareSelectedServices(ctx); e != nil {
			t.Fatal(e)
		}
		snapshot := f.owners.Components.Budget.Snapshot()
		t.Logf("selected HA preparation role=%d actual Used=%+v Classes=%+v RecoveryClaims=%d declaredRecovery=%d declaredReserve=%d", i, snapshot.Used, snapshot.Classes, snapshot.RecoveryClaims, cfg.ResourceBudget.RecoveryRAMBytes, cfg.ResourceBudget.RecoveryReserveRAMBytes)
		if e = f.prepareSelectedTransport(logger); e != nil {
			t.Fatal(e)
		}
	}
	for _, f := range owners[:1] {
		go func(f *haSelectedRuntimeFoundation) {
			if e := f.serveSelectedTransport(ctx); e != nil {
				t.Error(e)
			}
		}(f)
	}
	until := time.Now().Add(20 * time.Second)
	for !owners[0].replication.manager.PeerConnections()[request.Configs[1].Replication.NodeID] {
		if time.Now().After(until) {
			t.Fatal("actual TLS peer unavailable")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c := owners[0].reopened.Owners.Components
	now := time.Now().UTC()
	event := ledger.Event{EventID: "physical-ha-durable-create", Kind: ledger.EventWorkUnitCreated, WorkUnitID: "wku_physicaldelta", ProjectID: "project_physical_delta", KeyID: "key_physical_delta", OccurredAt: now, PeriodID: now.Format("2006-01-02"), PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, PeriodStartMicros: now.Truncate(24 * time.Hour).UnixMicro(), PeriodEndMicros: now.Truncate(24 * time.Hour).Add(24 * time.Hour).UnixMicro(), Operation: "workunit.create", IdempotencyKeyHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RequestFingerprint: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	e = c.Ledger.WithPreparedPhysicalAppend(ctx, func(ctx context.Context) error {
		return c.Ledger.WithNativeApplyWorkspace(ctx, func(ctx context.Context) error {
			_, _, e := c.NativeLedger.AppendApplyRecordWithReceipt(ctx, event, func(ctx context.Context, r ledger.Record) error {
				if r.Sequence != 1 {
					return authenticatedstate.ErrSealIneligible
				}
				// actual native Sync and original Ordering durability preceded this callback.
				if e := os.WriteFile(request.Ready, []byte("durable"), 0600); e != nil {
					return e
				}
				for {
					time.Sleep(100 * time.Millisecond)
				}
			})
			return e
		})
	})
	t.Fatal("durable callback unexpectedly returned", e)
}
