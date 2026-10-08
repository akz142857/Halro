package app

import (
	"bytes"
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

type haMetadataCrashRequest struct {
	Config config.Config
	Keys   HAMetadataKeys
	Ready  string
}

func TestHAMetadataMaintenanceSIGKILLHelper(t *testing.T) {
	path := os.Getenv("HALRO_HA_METADATA_CRASH_REQUEST")
	if path == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var request haMetadataCrashRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	guard, err := lock.Acquire(request.Config.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	owner, err := OpenHAMetadataMaintenance(context.Background(), request.Config, request.Keys, guard, functionalHAMetadataManager(t, request.Config), slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := owner.Run(context.Background(), func(s replication.MemberState) error {
		raw, err := json.Marshal(s)
		if err != nil {
			return err
		}
		return os.WriteFile(request.Ready, raw, 0600)
	}); err != nil {
		t.Fatal(err)
	}
}

func startHAMetadataCrashChild(t *testing.T, cfg config.Config, keys HAMetadataKeys) (*exec.Cmd, string, *bytes.Buffer) {
	t.Helper()
	directory := t.TempDir()
	requestPath := filepath.Join(directory, "request.json")
	ready := filepath.Join(directory, "confirmed.json")
	raw, err := json.Marshal(haMetadataCrashRequest{Config: cfg, Keys: keys, Ready: ready})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestHAMetadataMaintenanceSIGKILLHelper$", "-test.count=1")
	command.Env = append(os.Environ(), "HALRO_HA_METADATA_CRASH_REQUEST="+requestPath)
	output := &bytes.Buffer{}
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			command.Process.Kill()
			command.Wait()
		}
	})
	return command, ready, output
}

func killHAMetadataChild(t *testing.T, command *exec.Cmd, output *bytes.Buffer) {
	t.Helper()
	if err := command.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	err := command.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("actual child not killed: %v output=%s", err, output.String())
	}
	if state, ok := exit.Sys().(syscall.WaitStatus); !ok || !state.Signaled() || state.Signal() != syscall.SIGKILL {
		t.Fatalf("actual exit was not SIGKILL: %v output=%s", err, output.String())
	}
}

func awaitHAMetadataCrashCut(t *testing.T, cfg config.Config, keys HAMetadataKeys, predicate func(replication.MemberState) bool) replication.MemberState {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		state, key, err := readMemberState(cfg, keys.Master)
		clear(key[:])
		if err == nil && predicate(state) {
			return state
		}
		time.Sleep(5 * time.Millisecond)
	}
	state, key, err := readMemberState(cfg, keys.Master)
	clear(key[:])
	t.Fatal("child did not reach actual authenticated crash cut", state, err)
	return state
}

func TestHAMetadataMaintenanceActualSIGKILLPreservesContinuousAuthority(t *testing.T) {
	f := prepareHAMetadataMaintenanceFixture(t)
	prepareHAMetadataMaintenanceArtifacts(t, f)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	primaryChild, _, output := startHAMetadataCrashChild(t, f.configs[0], f.keys)
	// No Replica is listening: actual N+1 is source/order durable, while N+1
	// remains unconfirmed. Kill the owner without any graceful Close callback.
	before := awaitHAMetadataCrashCut(t, f.configs[0], f.keys, func(s replication.MemberState) bool {
		return s.DurableIndex == f.oldCut.DurableIndex+1 && s.ConfirmedIndex == f.oldCut.ConfirmedIndex
	})
	killHAMetadataChild(t, primaryChild, output)
	if before.Incarnation != f.oldCut.Incarnation || before.Term != f.oldCut.Term {
		t.Fatal(before)
	}
	primaryGuard, err := lock.Acquire(f.configs[0].Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	primary, err := OpenHAMetadataMaintenance(context.Background(), f.configs[0], f.keys, primaryGuard, functionalHAMetadataManager(t, f.configs[0]), logger)
	if err != nil {
		t.Fatal("Primary crash reopen", err)
	}
	replicaChild, ready, replicaOutput := startHAMetadataCrashChild(t, f.configs[1], f.keys)
	runCtx, stop := context.WithTimeout(context.Background(), 20*time.Second)
	primaryDone := make(chan error, 1)
	go func() { primaryDone <- primary.Run(runCtx, nil) }()
	after := awaitHAMetadataCrashCut(t, f.configs[1], f.keys, func(s replication.MemberState) bool {
		_, err := os.Stat(ready)
		return err == nil && s.AppliedIndex == f.oldCut.DurableIndex+1 && s.ConfirmedIndex == s.AppliedIndex
	})
	if after.Projection.MetadataEpoch != f.oldCut.Projection.MetadataEpoch+1 || after.Projection.MetadataSequence != 0 {
		t.Fatal(after)
	}
	scratch := filepath.Join(f.configs[1].Storage.DataDir, HAMetadataBaselineDirectory, "ledger-maintenance.0", "projection.db")
	opaque, err := os.ReadFile(scratch)
	if err != nil {
		t.Fatal("actual scratch missing before SIGKILL", err)
	}
	killHAMetadataChild(t, replicaChild, replicaOutput)
	stop()
	<-primaryDone
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}
	if err := primaryGuard.Close(); err != nil {
		t.Fatal(err)
	}
	// A killed Replica's DB never gets reopened or deleted. A new fixed slot
	// reconstructs its applied Ledger prefix from real native/ordering evidence.
	owners := [2]*HAMetadataMaintenance{}
	guards := [2]*lock.Lock{}
	for i, cfg := range f.configs {
		guards[i], err = lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer guards[i].Close()
		owners[i], err = OpenHAMetadataMaintenance(context.Background(), cfg, f.keys, guards[i], functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			t.Fatal("post-crash real owner", i, err)
		}
		defer owners[i].Close()
	}
	preserved, err := os.ReadFile(scratch)
	if err != nil || !bytes.Equal(preserved, opaque) {
		t.Fatal("crashed unqualified derivative was read/mutated/removed", err)
	}
	if filepath.Base(owners[1].ledgerScratch.path) != "ledger-maintenance.1" {
		t.Fatal("Replica reused killed derivative", owners[1].ledgerScratch.path)
	}
	runCtx, stop = context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	done := make(chan error, 2)
	for _, owner := range owners {
		go func(m *HAMetadataMaintenance) { done <- m.Run(runCtx, nil) }(owner)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		connected := owners[0].member.manager.PeerConnections()["halro-1"] && owners[1].member.manager.PeerConnections()["halro-0"]
		if connected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("post-SIGKILL real TLS sessions did not reconnect")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for i, owner := range owners {
		state := owner.Snapshot()
		if state.Incarnation != f.oldCut.Incarnation || state.Term != 7 || state.DurableIndex != f.oldCut.DurableIndex+1 || state.AppliedIndex != state.DurableIndex {
			t.Fatal("crash restart reset or duplicated ordering", i, state)
		}
	}
	stop()
	<-done
	<-done
	for _, owner := range owners {
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
	}
	preserved, err = os.ReadFile(scratch)
	if err != nil || !bytes.Equal(preserved, opaque) {
		t.Fatal("normal close retired another process's opaque slot", err)
	}
}
