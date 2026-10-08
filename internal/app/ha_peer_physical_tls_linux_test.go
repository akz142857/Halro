//go:build linux

package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
	"golang.org/x/sys/unix"
	"log/slog"
	"os"
	"testing"
	"time"
)

// Actual TLS plus original prepaid vector/bookkeeping. This test deliberately
// does not claim public Default Runtime Ready or sustained bank renewal.
func TestHAPhysicalPeerCreditActualTLSPreacceptAndUnusedRelease(t *testing.T) {
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
		if i == 1 {
			if err := f.reopened.Owners.Components.Metadata.PrepareHAReplicaPhysicalEscrow(ctx, 2); err != nil {
				t.Fatal("actual pre-exposure Metadata receive window", err)
			}
		}
		if err := f.prepareSelectedTransportWithPhysicalCredit(logger); err != nil {
			t.Fatal("actual transport consumer", i, err)
		}
	}

	primary, replica := owners[0], owners[1]
	pause := &haPeerPhysicalPausedProjection{next: &haSelectedReplicaProjection{owner: replica}, entered: make(chan struct{}, 1), exit: make(chan struct{})}
	installPeerPhysicalPausedTransport(t, ctx, replica, pause, logger)
	q := primary.replication.peerPhysical.Load()
	rq := replica.replication.peerPhysical.Load()
	if q == nil || rq == nil {
		t.Fatal("original complete physical producer absent")
	}
	ph, pe := primary.selectedHello()
	rh, re := replica.selectedHello()
	if pe != nil || re != nil || ph.Protocol.Current != 3 || rh.Protocol.Current != 3 {
		t.Fatal("strict capability not negotiated")
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 2)
	for _, f := range owners {
		go func(f *haSelectedRuntimeFoundation) { done <- f.serveSelectedTransport(runCtx) }(f)
	}
	defer func() {
		stop()
		for range 2 {
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
		for _, f := range owners {
			if err := f.replication.manager.Close(); err != nil {
				t.Error(err)
			}
		}
		for _, f := range owners {
			if err := f.replication.peerPhysical.Load().closeJoined(ctx); err != nil {
				t.Error("actual peer controller joined", err)
			}
		}
	}()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		q.mu.Lock()
		ready := q.peers[0].window.Operation == replication.PhysicalCreditWindow
		q.mu.Unlock()
		if ready && primary.replication.startupReady.Load() {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("actual TLS Window or ordinary ACK absent")
		case <-time.After(10 * time.Millisecond):
		}
	}
	before := primary.replication.publisher.Snapshot()
	// Ordinary context does not own the peer future ticket and cannot authorize
	// any Primary native byte. A forged scalar message is not a substitute.
	if err := q.beforePrimarySource(ctx, 0, 1, 1, 1); !errors.Is(err, replication.ErrPhysicalCredit) {
		t.Fatal("unowned native accepted", err)
	}
	cost, err := haPeerPhysicalTerminalCost(2)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		key := sha256.Sum256([]byte{byte(n + 1)})
		capCtx, ticket, err := q.reserve(ctx, key, cost)
		if err != nil {
			t.Fatal("genuine preaccept peer reserve", n, err)
		}
		if capCtx == ctx || ticket == nil {
			t.Fatal("private original ticket absent")
		}
		// The startup replenisher cannot become a live allocation path or clear
		// this actual accepted peer obligation once transport has been installed.
		if err := primary.replenishHAPhysicalStartupOrdering(ctx); !errors.Is(err, authenticatedstate.ErrSealIneligible) {
			t.Fatal("startup renewal escaped into live transport", err)
		}
		if err := primary.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
			return primary.withSelectedCut(ctx, func(ctx context.Context, cut statecut.HASelectedCut) error {
				return statecut.RequireNoHAOrderingPeerPromises(ctx, primary.owners, primary.physicalPublication)
			})
		}); !errors.Is(err, replication.ErrPhysicalCredit) {
			t.Fatal("common publication bypassed live future", err)
		}
		if err := q.finishTicket(ctx, ticket); err != nil {
			t.Fatal("actual all-peer known unused release", n, err)
		}
		if err := q.beforePrimarySource(capCtx, 0, 1, 1, 1); !errors.Is(err, replication.ErrPhysicalCredit) {
			t.Fatal("escaped ticket retained native permission", err)
		}
	}
	after := primary.replication.publisher.Snapshot()
	if before.DurableIndex != after.DurableIndex || before.OrderingHeadMAC != after.OrderingHeadMAC {
		t.Fatal("IO-only exchange changed native/Ordering cut")
	}
	tooLarge, err := haPeerPhysicalTerminalCost(4)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.reserve(ctx, sha256.Sum256([]byte("capacity")), tooLarge); !errors.Is(err, replication.ErrPhysicalCredit) {
		t.Fatal("unfunded real Metadata bank admitted", err)
	}
	q.mu.Lock()
	owned := q.live
	q.mu.Unlock()
	if owned != nil {
		t.Fatal("known preaccept refusal retained unused ticket")
	}

	capCtx, ticket, err := q.reserve(ctx, sha256.Sum256([]byte("actual-metadata-pause")), cost)
	if err != nil {
		t.Fatal(err)
	}
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- primary.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(capCtx, func(ctx context.Context) error {
			_, err := primary.metadataStore.PutProject(ctx, domain.Project{ID: "prj_peer_credit", Name: "Original credit", Enabled: true}, 0, nil)
			return err
		})
	}()
	defer func() {
		select {
		case <-pause.exit:
		default:
			close(pause.exit)
		}
	}()
	select {
	case <-pause.entered:
	case <-ctx.Done():
		t.Fatal("actual confirmed applier absent", ctx.Err())
	}
	beforeFee := q.fee
	waitCtx, cancelWait := context.WithTimeout(ctx, 60*time.Millisecond)
	// This original cursor was captured by the live ticket's actual whole
	// source scope. The coordinator independently matches it to the current
	// complete Ordering prefix, including the trailing Metadata fact. It does
	// not borrow the Directory pool still owned by the real Metadata writer.
	target, targetErr := primary.replication.coordinator.CaptureRecoveryTarget(waitCtx, ticket.startAllowed[0])
	if targetErr != nil {
		t.Fatal("actual complete ordering target", targetErr)
	}
	err = primary.replication.coordinator.WaitRecoveryAppliedTarget(waitCtx, target)
	cancelWait()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("trailing original Metadata Apply returned completion", err)
	}
	q.mu.Lock()
	held := q.live == ticket
	q.mu.Unlock()
	if !held {
		t.Fatal("unknown completion forgot original persistent ticket")
	}
	if _, err := beforeFee.Cost(); err != nil {
		t.Fatal("unknown completion retired original paid owner", err)
	}
	close(pause.exit)
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatal("actual Metadata owner did not join", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := q.finishTicket(ctx, ticket); err != nil {
		t.Fatal("actual applier exit did not join and release original ticket", err)
	}
	if got, err := replica.metadataStore.GetProject(ctx, "prj_peer_credit"); err != nil || got.ID != "prj_peer_credit" {
		t.Fatal("actual Replica metadata missing", got, err)
	}
	if err := q.withAdmissionPaused(ctx, func(ctx context.Context) error {
		if _, _, err := q.reserve(ctx, sha256.Sum256([]byte("paused")), cost); !errors.Is(err, replication.ErrPhysicalCredit) {
			return errors.New("new future entered actual maintenance gate")
		}
		return nil
	}); err != nil {
		t.Fatal("original Primary paused control gate", err)
	}
	if err := rq.withAdmissionPaused(ctx, func(ctx context.Context) error {
		if _, err := rq.replicaCredit(ctx, nil); !errors.Is(err, replication.ErrPhysicalCredit) {
			return errors.New("new Window entered actual receive/apply maintenance gate")
		}
		return nil
	}); err != nil {
		t.Fatal("original Replica paused control and actual receive gate", err)
	}
	for _, f := range owners {
		if err := f.finishHAPhysicalBusiness(ctx); err != nil {
			t.Fatal("actual common and paid owner join", err)
		}
		if err := f.closeHAPhysicalNative(ctx); err != nil {
			t.Fatal("original native actual retirement", err)
		}
	}
}

type haPeerPhysicalPausedProjection struct {
	next    *haSelectedReplicaProjection
	entered chan struct{}
	exit    chan struct{}
}

func (p *haPeerPhysicalPausedProjection) ApplyLedgerThrough(ctx context.Context, g, s uint64) error {
	return p.next.ApplyLedgerThrough(ctx, g, s)
}
func (p *haPeerPhysicalPausedProjection) ApplyMetadataThrough(ctx context.Context, g, s uint64) error {
	return p.next.ApplyMetadataThrough(ctx, g, s)
}
func (p *haPeerPhysicalPausedProjection) ApplyConfirmedPrefix(ctx context.Context, prefix *replication.ReplicaConfirmedPrefix) (replication.ProjectionState, error) {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	<-p.exit // Deliberately ignore cancellation while this real original callback is in IO.
	return p.next.ApplyConfirmedPrefix(ctx, prefix)
}
func installPeerPhysicalPausedTransport(t *testing.T, ctx context.Context, f *haSelectedRuntimeFoundation, p *haPeerPhysicalPausedProjection, logger *slog.Logger) {
	t.Helper()
	r := f.replication
	if err := r.manager.Close(); err != nil {
		t.Fatal(err)
	}
	r.manager = nil
	var err error
	r.applier, err = replication.NewReplicaApplier(r.receiver, r.journal, p)
	if err != nil {
		t.Fatal(err)
	}
	h, err := replication.NewReplicaRecordHandler(r.receiver, r.applier)
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
	for _, p := range state.Peers {
		endpoints = append(endpoints, replication.PeerEndpoint{NodeID: p.Name, Address: p.Address, SPKISHA256: p.SPKISHA256})
	}
	r.manager, err = replication.NewConnectionManager(replication.ConnectionManagerOptions{LocalNode: state.NodeID, TLS: replication.TLSFiles{CAFile: f.cfg.Replication.TLS.CAFile, CertFile: f.cfg.Replication.TLS.CertFile, KeyFile: f.cfg.Replication.TLS.KeyFile}, ClusterKey: r.clusterKey[:], Peers: endpoints, LocalHello: f.selectedHello, OnAuthenticatedHello: f.authorizeSelectedHello, Handle: f.selectedHandleRecords(h.Handle), Session: session, OnDataSessionReady: func(node string, peer replication.Hello, send func([]byte) error) error {
		if err := f.sendSelectedReplicaReadyAck(node, peer, send); err != nil {
			return err
		}
		return r.peerPhysical.Load().sendReplicaWindow(f.ctx, send)
	}, OnSessionError: func(peer string, err error) { logger.Warn("actual peer paused session", "peer", peer, "error", err) }})
	if err != nil {
		t.Fatal(err)
	}
	f.sender.Set(r.manager)
}
