package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestHAMemoryPendingMetadataSIGKILLChild(t *testing.T) {
	path := os.Getenv("HALRO_HA_MEMORY_PENDING_CHILD")
	if path == "" {
		t.Skip("actual pending-write child")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var request haMetadataCrashRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	clear(raw)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	guard, err := lock.Acquire(request.Config.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	a, r, err := openHARuntimeSavedPrefix(ctx, request.Config, request.Keys, guard, functionalHAMetadataManager(t, request.Config), slog.New(slog.NewTextHandler(io.Discard, nil)), haMemoryStateBootstrapPath(request.Config))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := r.ResumeSelectedMetadata(ctx); err != nil {
		t.Fatal(err)
	}
	err = a.maintenance.Run(ctx, func(replication.MemberState) error {
		// No Replica is serving. The real coordinator refuses confirmation after
		// native+ordering durability; retain this process until the parent kills
		// it. Do not fabricate an available peer to keep a waiter pending.
		_, err := a.maintenance.baseline.Update(ctx, func(tx *metadatastate.Transaction) error {
			return tx.Put([]string{"projects"}, []byte("killed-unconfirmed-write"), []byte("actual-native"))
		})
		if !errors.Is(err, replication.ErrReplicationUnavailable) {
			return errors.Join(errors.New("expected actual missing-peer confirmation refusal"), err)
		}
		t.Log("actual unconfirmed native write: confirmation refused; awaiting SIGKILL")
		<-ctx.Done()
		return ctx.Err()
	})
	t.Fatal("pending writer returned before actual SIGKILL", err)
}

func TestHAMemoryPendingPrimaryActualSIGKILLReopensSavedSevenRoots(t *testing.T) {
	f := prepareHAMetadataMaintenanceFixture(t)
	prepareHAMetadataMaintenanceArtifacts(t, f)
	completeHAMemoryActivationBoundary(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg := f.configs[0]
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := ActivateHAMemoryRuntime(ctx, cfg, f.keys, guard, functionalHAMetadataManager(t, cfg), logger)
	if err != nil {
		t.Fatal(err)
	}
	initial := a.Receipt
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pending-child.json")
	raw, err := json.Marshal(haMetadataCrashRequest{Config: cfg, Keys: f.keys})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	clear(raw)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHAMemoryPendingMetadataSIGKILLChild$", "-test.count=1", "-test.timeout=45s")
	cmd.Env = append(os.Environ(), "HALRO_HA_MEMORY_PENDING_CHILD="+path)
	output := &bytes.Buffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	durable := awaitHAMetadataCrashCut(t, cfg, f.keys, func(s replication.MemberState) bool {
		return s.DurableIndex == f.oldCut.DurableIndex+2 && s.ConfirmedIndex == f.oldCut.DurableIndex+1
	})
	if durable.AppliedIndex != durable.ConfirmedIndex || durable.Projection.MetadataSequence != 0 {
		t.Fatal("local unconfirmed metadata granted applied transfer permission", durable)
	}
	killHAMetadataChild(t, cmd, output)
	guard, err = lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	a, r, err := openHARuntimeSavedPrefix(ctx, cfg, f.keys, guard, functionalHAMetadataManager(t, cfg), logger, haMemoryStateBootstrapPath(cfg))
	if err != nil {
		t.Fatal("actual killed Primary prefix reopen", err)
	}
	defer a.Close()
	if r.Seal.Revision != initial.Revision || r.Seal.OrderingIndex != durable.ConfirmedIndex || r.Seal.Positions[1].Sequence != 0 {
		t.Fatal("unconfirmed native tail selected as old common root", r.Seal, durable)
	}
	if err := r.Owners.Components.Metadata.View(ctx, func(tx *metadatastate.Transaction) error {
		_, found, err := tx.Get([]string{"projects"}, []byte("killed-unconfirmed-write"))
		if found {
			return errors.New("unconfirmed tail exposed by saved common root")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.AdvanceFullHAPrefix(ctx); err != nil {
		t.Fatal("actual Primary local durable allowed-prefix reduction", err)
	}
	base := r.Seal
	base.Revision++
	pub, err := statecut.PublishHA(ctx, r.Owners, base)
	if err != nil {
		t.Fatal("actual Primary local unconfirmed seven-root checkpoint", err)
	}
	actual := a.maintenance.Snapshot()
	if pub.Seal.OrderingIndex != durable.DurableIndex || pub.Seal.Positions[1].Sequence != 1 || actual.ConfirmedIndex != durable.ConfirmedIndex || actual.AppliedIndex != actual.ConfirmedIndex || actual.Projection.MetadataSequence != 0 {
		t.Fatal("common local cut falsely advanced HA confirmation/applied", pub.Seal, actual)
	}
	// This is deliberately a local checkpoint, not Runtime business Ready:
	// the real coordinator has no quorum and cannot confirm its current target.
	waitCtx, stop := context.WithTimeout(ctx, 25*time.Millisecond)
	defer stop()
	if err := a.maintenance.member.coordinator.WaitConfirmed(waitCtx, actual.DurableIndex); !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, replication.ErrReplicationUnavailable) {
		t.Fatal("missing quorum falsely passed actual readiness confirmation", err)
	}
}
