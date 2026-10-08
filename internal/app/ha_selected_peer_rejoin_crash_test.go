package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/store/lock"
)

func prepareGenuinePeerRejoinFixture(t *testing.T, ctx context.Context) (haMetadataMaintenanceFixture, replication.MemberState, *haSelectedRuntimeFoundation) {
	t.Helper()
	fixture, owners, _ := prepareHASelectedTransportFixture(t, ctx)
	primary, candidate := owners[0], owners[1]
	before := candidate.replication.publisher.Snapshot()
	if err := candidate.Close(); err != nil {
		t.Fatal(err)
	}
	if err := candidate.owners.Components.Guard.Close(); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- primary.serveSelectedTransport(runCtx) }()
	joined := false
	t.Cleanup(func() {
		stop()
		if !joined {
			<-done
		}
	})
	cfg := fixture.configs[1]
	_, err := PromoteMember(controlledHAPromotionContext(t, ctx, cfg), cfg, PromoteMemberOptions{ExpectedTerm: before.Term, ExpectedAppliedIndex: before.AppliedIndex, OldPrimaryNodeID: fixture.configs[0].Replication.NodeID, FencedBy: replication.FenceNodeIsolated, Username: "admin", Password: []byte("correct horse battery staple")})
	if err != nil {
		t.Fatal("genuine peer TLS/public candidate prepare", err)
	}
	select {
	case <-primary.replication.fatalErrors():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stop()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	joined = true
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}
	if err := primary.owners.Components.Guard.Close(); err != nil {
		t.Fatal(err)
	}
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	f, err := openHASelectedRuntimeFoundation(ctx, cfg, fixture.keys.Master, guard, functionalHAMetadataManager(t, cfg), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		guard.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
		guard.Close()
	})
	if err := f.prepareSelectedServices(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.prepareSelectedTransport(slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	serveCtx, cancel := context.WithCancel(ctx)
	serveDone := make(chan error, 1)
	go func() { serveDone <- f.serveSelectedTransport(serveCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-serveDone; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	return fixture, before, f
}

func TestHASelectedPeerRejoinSIGKILLChild(t *testing.T) {
	path := os.Getenv("HALRO_HA_PEER_REJOIN_CRASH_INPUT")
	if path == "" {
		t.Skip("actual SIGKILL child only")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	if err = json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	master, err := unlockMemberMasterKey(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(master)
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	wanted := os.Getenv("HALRO_HA_PEER_REJOIN_CRASH_PHASE")
	_, err = rejoinHASelectedMemberSampled(ctx, cfg, master, guard, functionalHAMetadataManager(t, cfg), slog.New(slog.NewTextHandler(io.Discard, nil)), func(phase string) error {
		if phase == wanted {
			if e := syscall.Kill(os.Getpid(), syscall.SIGKILL); e != nil {
				return e
			}
			select {}
		}
		return nil
	})
	t.Fatal("actual crash phase not reached", wanted, err)
}

func TestHASelectedPeerRejoinActualSIGKILLRestoresOnlyOwnLeadership(t *testing.T) {
	for _, phase := range []string{"peer-candidate-carrier", "peer-adopted", "peer-leadership-received", "peer-leadership-applied", "audit-capsule", "governance-capsule", "peer-published"} {
		t.Run(phase, func(t *testing.T) {
			stressPhase(t, phase, "peer-candidate-carrier", "peer-leadership-applied", "peer-published")
			ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
			defer cancel()
			fixture, before, _ := prepareGenuinePeerRejoinFixture(t, ctx)
			cfg := fixture.configs[0]
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			input := filepath.Join(t.TempDir(), "peer-rejoin-input.json")
			if err = os.WriteFile(input, raw, 0600); err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHASelectedPeerRejoinSIGKILLChild$", "-test.count=1", "-test.timeout=50s")
			command.Env = append(os.Environ(), "HALRO_HA_PEER_REJOIN_CRASH_INPUT="+input, "HALRO_HA_PEER_REJOIN_CRASH_PHASE="+phase)
			output, err := command.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatal("child did not reach real SIGKILL", err, string(output))
			}
			status, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("not actual SIGKILL", err, string(output))
			}
			resumed, err := RejoinHASelectedMember(controlledHAPromotionContext(t, ctx, cfg), cfg)
			if err != nil {
				t.Fatal("fresh independently qualified peer restart", err, string(output))
			}
			if resumed.Role != replication.RoleReplica || resumed.Term != before.Term+1 || resumed.PromisedTerm != resumed.Term || resumed.DurableIndex != before.AppliedIndex+1 || resumed.ConfirmedIndex != resumed.DurableIndex || resumed.AppliedIndex != resumed.DurableIndex || resumed.Projection.MetadataEpoch != before.Projection.MetadataEpoch || resumed.Projection.MetadataSequence != before.Projection.MetadataSequence {
				t.Fatal("rejoin relabeled old root or admitted semantic suffix", resumed)
			}
			guard, err := lock.Acquire(cfg.Storage.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			fresh, err := openHASelectedRuntimeFoundation(ctx, cfg, fixture.keys.Master, guard, functionalHAMetadataManager(t, cfg), slog.New(slog.NewTextHandler(io.Discard, nil)))
			if fresh != nil {
				defer fresh.Close()
			}
			if err != nil {
				t.Fatal("ordinary post-rejoin factory", err)
			}
			if err := fresh.prepareSelectedRoots(ctx); err != nil {
				t.Fatal("actual new-role seven roots", err)
			}
			if fresh.peerPromotionRecovery != nil || fresh.promotionRecovery != nil || fresh.reopened.Seal.Term != resumed.Term || fresh.reopened.Seal.OrderingIndex != resumed.AppliedIndex {
				t.Fatal("cold authority retained after new common")
			}
		})
	}
}
