//go:build linux

package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/nativephysical"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
	"golang.org/x/sys/unix"
)

type haPhysicalObservedPair struct {
	beforeLedger, afterLedger     ledger.HAReplicaPhysicalObservation
	beforeMetadata, afterMetadata metadatastate.HAReplicaPhysicalObservation
}

// The wrapper observes the genuine applier's opaque confirmed job and then
// delegates unchanged. It never constructs a prefix, edits Member progress or
// makes the observation a source/role/admission permission.
type haPhysicalObservedProjection struct {
	owner *haSelectedRuntimeFoundation
	next  *haSelectedReplicaProjection
	pairs chan haPhysicalObservedPair
}

func (p *haPhysicalObservedProjection) ApplyLedgerThrough(ctx context.Context, gen, seq uint64) error {
	return p.next.ApplyLedgerThrough(ctx, gen, seq)
}
func (p *haPhysicalObservedProjection) ApplyMetadataThrough(ctx context.Context, gen, seq uint64) error {
	return p.next.ApplyMetadataThrough(ctx, gen, seq)
}
func (p *haPhysicalObservedProjection) ApplyConfirmedPrefix(ctx context.Context, prefix *replication.ReplicaConfirmedPrefix) (replication.ProjectionState, error) {
	c := p.owner.reopened.Owners.Components
	var pair haPhysicalObservedPair
	observe := func(l *ledger.HAReplicaPhysicalObservation, m *metadatastate.HAReplicaPhysicalObservation) error {
		return c.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
			cost := c.Budget.Snapshot().Used
			var err error
			*l, err = c.Ledger.ObserveHAReplicaPhysicalCatchup(ctx, c.NativeLedger)
			if err != nil {
				return fmt.Errorf("original Ledger getter: %w", err)
			}
			*m, err = c.Metadata.ObserveHAReplicaPhysicalEscrow(ctx)
			if err != nil {
				return fmt.Errorf("original Metadata getter: %w", err)
			}
			if c.Budget.Snapshot().Used != cost {
				return errors.New("readonly observation changed paid fees")
			}
			return nil
		})
	}
	if err := observe(&pair.beforeLedger, &pair.beforeMetadata); err != nil {
		return replication.ProjectionState{}, err
	}
	result, err := p.next.ApplyConfirmedPrefix(ctx, prefix)
	if err != nil {
		return result, err
	}
	if err := observe(&pair.afterLedger, &pair.afterMetadata); err != nil {
		return result, err
	}
	select {
	case p.pairs <- pair:
		return result, nil
	case <-ctx.Done():
		return result, ctx.Err()
	}
}

func TestHAReplicaPhysicalDomainObservationActualReceivedAndApplied(t *testing.T) {
	root := os.Getenv("HALRO_EXT4_ROOT")
	if root == "" {
		t.Skip("requires isolated ext4 with original 32-frame bank capacity")
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC {
		t.Fatal("actual ext4", err)
	}
	if uint64(fs.Blocks)*uint64(fs.Bsize) < 1400<<20 {
		t.Skip("original full 32-frame Ledger and Usage vector requires at least 1400 MiB usable ext4 capacity (1536 MiB backing)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture := prepareHAMetadataMaintenanceFixture(t)
	prepareHAMetadataMaintenanceArtifacts(t, fixture)
	completeHAMemoryActivationBoundary(t, fixture)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	for _, cfg := range fixture.configs {
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		activation, err := ActivateHAMemoryRuntime(ctx, cfg, fixture.keys, guard, functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(activation.Close(), guard.Close()); err != nil {
			t.Fatal(err)
		}
	}
	var owners [2]*haSelectedRuntimeFoundation
	for i, cfg := range fixture.configs {
		limits := *cfg.ResourceBudget
		limits.TotalRAMBytes = 2 << 30
		limits.RecoveryRAMBytes, limits.RecoveryReserveRAMBytes = 896<<20, 896<<20
		limits.CompletionRAMBytes = 1 << 30
		limits.TotalUnconfirmedBytes = 64 << 20
		limits.IOUnconfirmedBytes = 32 << 20
		limits.CompletionUnconfirmedBytes = 32 << 20
		limits.RecoveryUnconfirmedBytes = 16 << 20
		limits.RecoveryReserveUnconfirmedBytes = 16 << 20
		limits.UnconfirmedMaxEntries = 64
		limits.MaxClaims += 32
		limits.ControlRAMBytes += 32 * resourcebudget.ClaimRAMBytes
		limits.PhysicalTerminalReserve = true
		limits.LedgerProjectionDiskBytes = 80 << 20
		limits.LedgerSegmentIndexDiskBytes = 48 << 20
		limits.UsageProjectionDiskBytes = 128 << 20
		if i == 1 {
			limits.LedgerProjectionDiskBytes = 576 << 20
			limits.UsageProjectionDiskBytes = 576 << 20
		}
		cfg.ResourceBudget = &limits
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		f, err := openHASelectedRuntimeFoundation(ctx, cfg, fixture.keys.Master, guard, functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			t.Fatal("actual foundation", i, err)
		}
		owners[i] = f
		t.Cleanup(func() {
			if err := f.Close(); err != nil {
				t.Error("actual foundation join", i, err)
			}
			if err := guard.Close(); err != nil {
				t.Error(err)
			}
		})
		if err := f.prepareSelectedServicesWithLease(ctx, i == 0); err != nil {
			t.Fatal("actual original vector", i, err)
		}
		if err := f.installHASelectedGovernanceService(ctx); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := f.prepareHASelectedPrimaryStartup(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.prepareSelectedTransport(logger); err != nil {
			t.Fatal("actual transport consumer", i, err)
		}
	}
	primary, replica := owners[0], owners[1]
	c := replica.reopened.Owners.Components
	// Explicit original two-frame Metadata bank: this test observes component
	// permissions and does not claim whole Runtime Ready.
	if err := c.Metadata.PrepareHAReplicaPhysicalEscrow(ctx, 2); err != nil {
		t.Fatal("original Replica Metadata escrow", err)
	}
	var initialLedger ledger.HAReplicaPhysicalObservation
	var initialMetadata metadatastate.HAReplicaPhysicalObservation
	if err := c.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		var err error
		initialLedger, err = c.Ledger.ObserveHAReplicaPhysicalCatchup(ctx, c.NativeLedger)
		if err != nil {
			return err
		}
		initialMetadata, err = c.Metadata.ObserveHAReplicaPhysicalEscrow(ctx)
		return err
	}); err != nil {
		t.Fatal("initial original bank", err)
	}
	if initialLedger.AvailableRecords != resourcebudget.MaxCompletionBatch || initialMetadata.AvailableFrames == 0 {
		t.Fatal("original bank absent", initialLedger, initialMetadata)
	}

	if err := c.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		used := c.Budget.Snapshot().Used
		audit, err := c.Audit.ObserveHAReplicaPhysicalNative(ctx)
		if err != nil || audit.Identity == ([32]byte{}) || audit.AvailableBytes <= 0 {
			return fmt.Errorf("actual original Audit bank: %w", err)
		}
		gov, err := c.Governance.ObserveHAReplicaPhysicalProjection(ctx)
		if err != nil || gov.Window.Selection == ([32]byte{}) || gov.AvailableFrames != 2 {
			return fmt.Errorf("actual original Governance bank: %w", err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		a, err := c.Audit.ObserveHAReplicaPhysicalNative(cancelled)
		if !errors.Is(err, context.Canceled) || a != (nativephysical.PreparedObservation{}) {
			return fmt.Errorf("canceled Audit observation: %w", err)
		}
		g, err := c.Governance.ObserveHAReplicaPhysicalProjection(cancelled)
		if !errors.Is(err, context.Canceled) || g != (governance.HAReplicaPhysicalObservation{}) {
			return fmt.Errorf("canceled Governance observation: %w", err)
		}
		if used != c.Budget.Snapshot().Used {
			return errors.New("Audit/Governance getters changed original paid fees")
		}
		t.Logf("actual original Audit available_bytes=%d; Governance roots=%d available_frames=%d", audit.AvailableBytes, gov.Window.UnusedRoots, gov.AvailableFrames)
		return nil
	}); err != nil {
		t.Fatal("Audit/Governance actual replica consumers", err)
	}
	pc0 := primary.reopened.Owners.Components
	if err := pc0.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		used := pc0.Budget.Snapshot().Used
		a, err := pc0.Audit.ObserveHAReplicaPhysicalNative(ctx)
		if err == nil || a != (nativephysical.PreparedObservation{}) {
			return errors.New("Primary Audit granted Replica observation")
		}
		g, err := pc0.Governance.ObserveHAReplicaPhysicalProjection(ctx)
		if err == nil || g != (governance.HAReplicaPhysicalObservation{}) {
			return errors.New("Primary Governance granted Replica observation")
		}
		u, err := pc0.Usage.ObserveHAReplicaPhysicalCatchup(ctx, pc0.NativeLedger, pc0.Ledger)
		if err == nil || u.WindowCount != 0 || u.AvailableFrames != 0 {
			return errors.New("Primary Usage granted Replica observation")
		}
		if used != pc0.Budget.Snapshot().Used {
			return errors.New("non-Replica refusal changed fees")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	observeRemainingDerived := func(ctx context.Context) error {
		used := c.Budget.Snapshot().Used
		u, err := c.Usage.ObserveHAReplicaPhysicalCatchup(ctx, c.NativeLedger, c.Ledger)
		if err != nil || u.WindowCount == 0 {
			return fmt.Errorf("original Usage getter: %w", err)
		}
		d, err := c.Directory.ObserveHAReplicaPhysicalCatchup(ctx, c.NativeLedger, c.Ledger)
		if err != nil || d.Window.Selection == ([32]byte{}) {
			return fmt.Errorf("original Directory getter: %w", err)
		}
		if c.Budget.Snapshot().Used != used {
			return errors.New("Usage/Directory observations changed paid fees")
		}
		t.Logf("actual Usage windows=%d available_frames=%d; Directory roots=%d available_rolls=%d", u.WindowCount, u.AvailableFrames, d.Window.UnusedRoots, d.AvailableRolls)
		return nil
	}
	if err := c.Directory.WithPhysicalCatchupWorkspace(ctx, observeRemainingDerived); err != nil {
		t.Fatal("initial derived consumers", err)
	}
	observed := &haPhysicalObservedProjection{owner: replica, next: &haSelectedReplicaProjection{owner: replica}, pairs: make(chan haPhysicalObservedPair, 8)}
	installObservedHAReplicaTransport(t, ctx, replica, observed, logger)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 2)
	for _, f := range owners {
		go func(f *haSelectedRuntimeFoundation) { done <- f.serveSelectedTransport(runCtx) }(f)
	}
	joined := false
	defer func() {
		stop()
		if !joined {
			for range 2 {
				if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			}
		}
	}()
	for !primary.replication.startupReady.Load() {
		select {
		case err := <-primary.replication.fatal:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	// Real Primary DAO operation produces one source-qualified Metadata frame.
	if err := primary.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		_, err := primary.metadataStore.PutProject(ctx, domain.Project{ID: "prj_observed", Name: "Original observed", Enabled: true}, 0, nil)
		return err
	}); err != nil {
		t.Fatal("actual Metadata producer", err)
	}
	pc := primary.reopened.Owners.Components
	w, err := pc.NativeLedger.ReservePhysicalTerminals(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	event := ledger.Event{EventID: "observed-ha-create", Kind: ledger.EventWorkUnitCreated, WorkUnitID: "wku_observed", ProjectID: "prj_observed", KeyID: "key_observed", OccurredAt: now, PeriodID: now.Format("2006-01-02"), PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, PeriodStartMicros: now.Truncate(24 * time.Hour).UnixMicro(), PeriodEndMicros: now.Truncate(24 * time.Hour).Add(24 * time.Hour).UnixMicro(), Operation: "workunit.create", IdempotencyKeyHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RequestFingerprint: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	var receipt ledger.AppendReceipt
	err = pc.Ledger.WithPreparedPhysicalAppend(w.Bind(ctx), func(ctx context.Context) error {
		return pc.Ledger.WithNativeApplyWorkspace(ctx, func(ctx context.Context) error {
			_, r, e := pc.NativeLedger.AppendApplyRecordWithReceipt(ctx, event, func(ctx context.Context, r ledger.Record) error {
				_, e := pc.Ledger.AdvanceOneFromNative(ctx, pc.NativeLedger, ledger.ProjectionPrefix{Head: ledger.Watermark{Generation: r.Generation, Sequence: r.Sequence, Offset: r.Offset}, Hash: r.Hash})
				return e
			})
			receipt = r
			return e
		})
	})
	if err != nil {
		t.Fatal("actual Ledger producer", err)
	}
	if err := primary.replication.coordinator.WaitConfirmed(ctx, receipt.ReplicationIndex); err != nil {
		t.Fatal(err)
	}
	for replica.replication.publisher.Snapshot().AppliedIndex < receipt.ReplicationIndex {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	sawLedger, sawMetadata := false, false
	// Keep the real TLS applier alive for terminal join. These already-applied
	// jobs enqueued their observations before advancing the actual Applied ACK.
	for remaining := len(observed.pairs); remaining > 0; remaining-- {
		pair := <-observed.pairs

		for _, bank := range []struct {
			initial, before, after authenticatedstate.PhysicalTerminalWindowObservation
		}{
			{initialLedger.Window, pair.beforeLedger.Window, pair.afterLedger.Window},
			{initialMetadata.Window, pair.beforeMetadata.Window, pair.afterMetadata.Window},
		} {
			for _, actual := range []authenticatedstate.PhysicalTerminalWindowObservation{bank.before, bank.after} {
				if actual.Selection != bank.initial.Selection || actual.Scope != bank.initial.Scope || actual.Pack != bank.initial.Pack || actual.Parent != bank.initial.Parent || actual.Controls != bank.initial.Controls || actual.Native.File != bank.initial.Native.File || actual.Native.Parent != bank.initial.Native.Parent || actual.NativeEnd != bank.initial.NativeEnd || actual.COWEnd != bank.initial.COWEnd {
					t.Fatal("observation replaced original actual paid bank", bank)
				}
			}
		}
		t.Logf("actual received/applied Ledger unused_roots=%d->%d available_records=%d->%d native_seq=%d; Metadata unused_roots=%d->%d available_frames=%d->%d native_seq=%d", pair.beforeLedger.Window.UnusedRoots, pair.afterLedger.Window.UnusedRoots, pair.beforeLedger.AvailableRecords, pair.afterLedger.AvailableRecords, pair.beforeLedger.Native.Sequence, pair.beforeMetadata.Window.UnusedRoots, pair.afterMetadata.Window.UnusedRoots, pair.beforeMetadata.AvailableFrames, pair.afterMetadata.AvailableFrames, pair.beforeMetadata.Native.Sequence)
		ldelta := pair.beforeLedger.Native.Sequence - initialLedger.Native.Sequence
		mdelta := pair.beforeMetadata.Native.Sequence - initialMetadata.Native.Sequence
		if pair.beforeLedger.AvailableRecords != initialLedger.AvailableRecords-uint32(ldelta) || pair.beforeMetadata.AvailableFrames != initialMetadata.AvailableFrames-uint32(mdelta) {
			t.Fatal("received obligation credited spare", pair)
		}
		if pair.afterLedger.AvailableRecords != pair.beforeLedger.AvailableRecords || pair.afterMetadata.AvailableFrames != pair.beforeMetadata.AvailableFrames {
			t.Fatal("Applied restored consumed target", pair)
		}
		if pair.beforeLedger.Window.UnusedRoots > pair.beforeLedger.AvailableRecords {
			sawLedger = true
		}
		if pair.beforeMetadata.Window.UnusedRoots > pair.beforeMetadata.AvailableFrames {
			sawMetadata = true
		}
	}
	if !sawLedger || !sawMetadata {
		t.Fatal("actual received-unapplied boundary missing", sawLedger, sawMetadata)
	}
	if err := c.Directory.WithPhysicalCatchupWorkspace(ctx, observeRemainingDerived); err != nil {
		t.Fatal("Applied derived consumers", err)
	}
	if err := c.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		used := c.Budget.Snapshot().Used
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		l, e := c.Ledger.ObserveHAReplicaPhysicalCatchup(cancelled, c.NativeLedger)
		if !errors.Is(e, context.Canceled) || l != (ledger.HAReplicaPhysicalObservation{}) {
			return fmt.Errorf("canceled Ledger getter: %w", e)
		}
		m, e := c.Metadata.ObserveHAReplicaPhysicalEscrow(cancelled)
		if !errors.Is(e, context.Canceled) || m != (metadatastate.HAReplicaPhysicalObservation{}) {
			return fmt.Errorf("canceled Metadata getter: %w", e)
		}
		if c.Budget.Snapshot().Used != used {
			return errors.New("cancel changed paid fees")
		}
		for _, check := range []struct {
			path    string
			observe func() error
		}{
			{filepath.Join(haMemoryStateDirectory(replica.cfg), "ledger", "terminal.physical.0"), func() error { _, e := c.Ledger.ObserveHAReplicaPhysicalCatchup(ctx, c.NativeLedger); return e }},
			{filepath.Join(replica.bootstrap.MetadataRoot, "terminal.physical.0"), func() error { _, e := c.Metadata.ObserveHAReplicaPhysicalEscrow(ctx); return e }},
		} {
			raw, e := os.ReadFile(check.path)
			if e != nil {
				return e
			}
			saved := check.path + ".actual-test-owner"
			if e = os.Rename(check.path, saved); e != nil {
				return e
			}
			if e = os.WriteFile(check.path, raw, 0600); e != nil {
				return e
			}
			foreign, e := os.Stat(check.path)
			if e != nil {
				return e
			}
			e = check.observe()
			current, se := os.Stat(check.path)
			if e == nil || se != nil || !os.SameFile(foreign, current) || c.Budget.Snapshot().Used != used {
				return errors.Join(errors.New("foreign same-byte control was trusted/retired or changed fees"), e, se)
			}
			// Test owns both files; restore the original namespace for actual cleanup.
			if e = os.Remove(check.path); e != nil {
				return e
			}
			if e = os.Rename(saved, check.path); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal("readonly negative observations", err)
	}
	for i, f := range owners {
		if err := f.finishHAPhysicalBusiness(ctx); err != nil {
			t.Fatal("original complete common", i, err)
		}
		if err := f.closeHAPhysicalNative(ctx); err != nil {
			t.Fatal("actual native join", i, err)
		}
		if err := f.Close(); err != nil {
			t.Fatal("actual whole close", i, err)
		}
	}
	stop()
	for range 2 {
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	joined = true
}

func installObservedHAReplicaTransport(t *testing.T, ctx context.Context, f *haSelectedRuntimeFoundation, p *haPhysicalObservedProjection, logger *slog.Logger) {
	t.Helper()
	r := f.replication
	if err := r.manager.Close(); err != nil {
		t.Fatal(err)
	}
	r.manager = nil // original known Close joined; re-enter the real constructor gate
	var err error
	r.applier, err = replication.NewReplicaApplier(r.receiver, r.journal, p)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := replication.NewReplicaRecordHandler(r.receiver, r.applier)
	if err != nil {
		t.Fatal(err)
	}
	var session replication.PeerSessionOptions
	if err := f.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		var err error
		session, err = f.selectedReplicationSessionOptions(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	state := r.publisher.Snapshot()
	endpoints := make([]replication.PeerEndpoint, 0, len(state.Peers))
	for _, peer := range state.Peers {
		endpoints = append(endpoints, replication.PeerEndpoint{NodeID: peer.Name, Address: peer.Address, SPKISHA256: peer.SPKISHA256})
	}
	r.manager, err = replication.NewConnectionManager(replication.ConnectionManagerOptions{LocalNode: state.NodeID, TLS: replication.TLSFiles{CAFile: f.cfg.Replication.TLS.CAFile, CertFile: f.cfg.Replication.TLS.CertFile, KeyFile: f.cfg.Replication.TLS.KeyFile}, ClusterKey: r.clusterKey[:], Peers: endpoints, LocalHello: f.selectedHello, OnAuthenticatedHello: f.authorizeSelectedHello, Handle: f.selectedHandleRecords(handler.Handle), Session: session, OnDataSessionReady: f.sendSelectedReplicaReadyAck, OnSessionError: func(peer string, err error) { logger.Warn("observed actual HA session", "peer", peer, "error", err) }})
	if err != nil {
		t.Fatal(err)
	}
	f.sender.Set(r.manager)
}

var _ replication.ReplicaConfirmedPrefixProjection = (*haPhysicalObservedProjection)(nil)
