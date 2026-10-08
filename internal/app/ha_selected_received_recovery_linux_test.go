//go:build linux

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
)

type haReceivedCrashInput struct {
	Config config.Config
	Keys   HAMetadataKeys
	Ready  string
}

// A real authenticated Primary frame reaches the original Replica native and
// Ordering fsyncs. The child is SIGKILLed immediately before its original
// publisher updates Member. No callback error or direct watermark edit models
// the gap, and the fixed accepted receipt must precede the first native write.
func TestHASelectedReceivedOrderingActualTLSSIGKILLAndIndependentReceipt(t *testing.T) {
	if os.Getenv("HALRO_EXT4_ROOT") == "" {
		t.Skip("isolated actual ext4 runner required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fixture := prepareHAMetadataMaintenanceFixture(t)
	prepareHAMetadataMaintenanceArtifacts(t, fixture)
	completeHAMemoryActivationBoundary(t, fixture)
	for i, cfg := range fixture.configs {
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		activated, err := ActivateHAMemoryRuntime(ctx, cfg, fixture.keys, guard, functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			t.Fatal(err)
		}
		if err = errors.Join(activated.Close(), guard.Close()); err != nil {
			t.Fatal(err)
		}
		limits := *cfg.ResourceBudget
		limits.TotalRAMBytes = 2 << 30
		limits.RecoveryRAMBytes = 896 << 20
		limits.RecoveryReserveRAMBytes = 896 << 20
		limits.CompletionRAMBytes = 1 << 30
		limits.PhysicalTerminalReserve = true
		// Use the same finite two-node physical fixture profile as the actual
		// business test; legacy defaults do not contain the physical Directory
		// pool. This is a receipt/causality test, not a capacity acceptance run.
		limits.MaxClaims += 32
		limits.ControlRAMBytes += 32 * resourcebudget.ClaimRAMBytes
		limits.LedgerProjectionDiskBytes = 56 << 20
		limits.LedgerSegmentIndexDiskBytes = 48 << 20
		limits.UsageProjectionDiskBytes = 128 << 20
		cfg.ResourceBudget = &limits
		fixture.configs[i] = cfg
	}
	primaryCfg, replicaCfg := fixture.configs[0], fixture.configs[1]
	guard, err := lock.Acquire(primaryCfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	primary, err := openHASelectedRuntimeFoundation(ctx, primaryCfg, fixture.keys.Master, guard, functionalHAMetadataManager(t, primaryCfg), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := publishHAReceivedStructuralCommon(context.Background(), primary); err != nil {
			t.Error("actual Primary terminal maintenance", err)
		}
		if err := primary.closeHAPhysicalNative(context.Background()); err != nil {
			t.Error("actual Primary native source close", err)
		}
		if err := errors.Join(primary.Close(), guard.Close()); err != nil {
			t.Error(err)
		}
	}()
	if err = prepareHAReceivedStructuralTransport(ctx, primary); err != nil {
		t.Fatal(err)
	}
	if err = primary.prepareSelectedTransport(logger); err != nil {
		t.Fatal(err)
	}
	serveCtx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- primary.serveSelectedTransport(serveCtx) }()
	defer func() {
		stop()
		if served != nil {
			if err := <-served; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
	}()
	ready := filepath.Join(t.TempDir(), "received-before-member")
	input := filepath.Join(t.TempDir(), "received-input.json")
	raw, err := json.Marshal(haReceivedCrashInput{Config: replicaCfg, Keys: fixture.keys, Ready: ready})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(input, raw, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHASelectedReceivedOrderingSIGKILLChild$", "-test.count=1", "-test.timeout=75s")
	command.Env = append(os.Environ(), "HALRO_HA_RECEIVED_ORDERING_INPUT="+input)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	joined := false
	defer func() {
		if !joined {
			command.Process.Kill()
			<-waited
		}
	}()
	until := time.Now().Add(30 * time.Second)
	for !primary.replication.manager.PeerConnections()[replicaCfg.Replication.NodeID] {
		select {
		case err = <-waited:
			joined = true
			t.Fatal("child before actual TLS", err, output.String())
		default:
		}
		if time.Now().After(until) {
			t.Fatal("actual authenticated TLS not connected", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	commit, err := primary.replication.coordinator.RecordSchemaBoundary(45, 46)
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err = os.Stat(ready); err == nil {
			break
		}
		select {
		case err = <-waited:
			joined = true
			t.Fatal("child before received Ordering", err, output.String())
		default:
		}
		if time.Now().After(until) {
			t.Fatal("genuine pre-Member receive window not reached", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = <-waited
	joined = true
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatal("not actual process SIGKILL", err, output.String())
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal("not SIGKILL", status, output.String())
	}
	before, err := replication.ReadState(replicaCfg.ReplicationStatePath(), primary.replication.clusterKey[:])
	if err != nil {
		t.Fatal(err)
	}
	if before.DurableIndex+1 != commit.Index || before.ConfirmedIndex != before.DurableIndex || before.AppliedIndex != before.DurableIndex {
		t.Fatal("actual Member gap absent", before, commit.Index)
	}
	originalState, err := os.ReadFile(replicaCfg.ReplicationStatePath())
	if err != nil {
		t.Fatal(err)
	}
	originalOrdering, err := os.ReadFile(replicaCfg.OrderingJournalPath())
	if err != nil {
		t.Fatal(err)
	}
	receiptPath := replicaCfg.ReplicationStatePath() + ".physical.json"
	receipt, err := os.ReadFile(receiptPath)
	if err != nil || len(receipt) != 8192 {
		t.Fatal("missing bounded accepted pre-IO receipt", len(receipt), err)
	}
	// Fully durable native/Ordering bytes alone must not manufacture acceptance.
	// Erasing both accepted slots keeps the original static IO header and birth.
	erased := bytes.Clone(receipt)
	clear(erased[2048:])
	if err = os.WriteFile(receiptPath, erased, 0600); err != nil {
		t.Fatal(err)
	}
	refuseGuard, err := lock.Acquire(replicaCfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	refused, err := openHASelectedRuntimeFoundation(ctx, replicaCfg, fixture.keys.Master, refuseGuard, functionalHAMetadataManager(t, replicaCfg), logger)
	if err == nil {
		t.Fatal("complete Ordering without accepted receipt granted Member recovery")
	}
	if refused != nil {
		if closeErr := refused.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	if err = refuseGuard.Close(); err != nil {
		t.Fatal(err)
	}
	actualState, err := os.ReadFile(replicaCfg.ReplicationStatePath())
	if err != nil || !bytes.Equal(actualState, originalState) {
		t.Fatal("refusal changed actual Member", err)
	}
	actualOrdering, err := os.ReadFile(replicaCfg.OrderingJournalPath())
	if err != nil || sha256.Sum256(actualOrdering) != sha256.Sum256(originalOrdering) {
		t.Fatal("refusal repaired/dropped original Ordering", err)
	}
	if err = os.WriteFile(receiptPath, receipt, 0600); err != nil {
		t.Fatal(err)
	}
	recoveredGuard, err := lock.Acquire(replicaCfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := openHASelectedRuntimeFoundation(ctx, replicaCfg, fixture.keys.Master, recoveredGuard, functionalHAMetadataManager(t, replicaCfg), logger)
	if err != nil {
		t.Fatal("genuine accepted receipt fresh full source recovery", err, output.String())
	}
	if err = recovered.prepareSelectedRoots(ctx); err != nil {
		t.Fatal("original confirmed startup seven roots", err)
	}
	after := recovered.replication.publisher.Snapshot()
	if after.Role != before.Role || after.Term != before.Term || after.PromisedTerm != before.PromisedTerm || after.DurableIndex != commit.Index || after.ConfirmedIndex != commit.Index || after.AppliedIndex != commit.Index || after.Projection.MetadataEpoch != before.Projection.MetadataEpoch || after.Projection.MetadataSequence != before.Projection.MetadataSequence {
		t.Fatal("recovery fabricated role/native permissions", before, after)
	}
	if err = recovered.prepareHAPhysicalPublicationTargets(ctx); err != nil {
		t.Fatal("actual restored common/cache targets", err)
	}
	if err = recovered.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		var openErr error
		recovered.metadataStore, openErr = recovered.reopened.OpenHAMetadataStore(ctx)
		return openErr
	}); err != nil {
		t.Fatal("recovered actual Metadata facade", err)
	}
	if err = recovered.prepareSelectedTransport(logger); err != nil {
		t.Fatal("recovered actual transport", err)
	}
	receivedCtx, stopReceived := context.WithCancel(ctx)
	receivedDone := make(chan error, 1)
	go func() { receivedDone <- recovered.serveSelectedTransport(receivedCtx) }()
	// Wait for the original authenticated session callback and actual ACK to
	// publish confirmation; WaitConfirmed legitimately rejects a disconnected
	// peer before the reconnect has registered its data session.
	for primary.replication.publisher.Snapshot().ConfirmedIndex < commit.Index {
		select {
		case <-ctx.Done():
			stopReceived()
			<-receivedDone
			t.Fatal("recovered genuine Ready ACK confirms Primary", ctx.Err())
		case err = <-receivedDone:
			stopReceived()
			t.Fatal("recovered original TLS stopped before actual ACK", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err = primary.replication.coordinator.WaitConfirmed(ctx, commit.Index); err != nil {
		stopReceived()
		<-receivedDone
		t.Fatal("actual persisted confirmation did not satisfy original coordinator", err)
	}
	stopReceived()
	if err = <-receivedDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	stop()
	if err = <-served; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	served = nil
	if err = publishHAReceivedStructuralCommon(ctx, recovered); err != nil {
		t.Fatal("actual applied seven-root publication and original retirement", err)
	}
	if err = recovered.closeHAPhysicalNative(ctx); err != nil {
		t.Fatal("original actual Replica native close", err)
	}
	if err = errors.Join(recovered.Close(), recoveredGuard.Close()); err != nil {
		t.Fatal("actual reconstructed owner joins", err)
	}
	freshGuard, err := lock.Acquire(replicaCfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := openHASelectedRuntimeFoundation(ctx, replicaCfg, fixture.keys.Master, freshGuard, functionalHAMetadataManager(t, replicaCfg), logger)
	if err != nil {
		t.Fatal("ordinary independent fresh Member/full source qualification", err)
	}
	if err = errors.Join(fresh.Close(), freshGuard.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestHASelectedReceivedOrderingSIGKILLChild(t *testing.T) {
	path := os.Getenv("HALRO_HA_RECEIVED_ORDERING_INPUT")
	if path == "" {
		t.Skip("actual SIGKILL child only")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input haReceivedCrashInput
	if err = json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cfg := input.Config
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	f, err := openHASelectedRuntimeFoundation(ctx, cfg, input.Keys.Master, guard, functionalHAMetadataManager(t, cfg), logger)
	if err != nil {
		t.Fatal(err)
	}
	if err = prepareHAReceivedStructuralTransport(ctx, f); err != nil {
		t.Fatal(err)
	}
	r := f.replication
	old := r.publisher.Snapshot()
	r.receiver, err = replication.NewReplicaStartupReceiver(old.ClusterID, old.Incarnation, old.NodeID, f.owners.Baseline.CurrentPrimary(), old.Term, old.PromisedTerm, old.ConfirmedIndex, old.AppliedIndex, r.journal, replication.ReplicaReceiverOptions{Projection: old.Projection, PersistState: func(progress replication.ReplicaProgress) error {
		if progress.DurableIndex > old.DurableIndex {
			index, term, mac := r.journal.Head()
			if index != progress.DurableIndex || term != old.Term || mac != progress.OrderingHeadMAC || r.publisher.Snapshot().DurableIndex != old.DurableIndex {
				return fmt.Errorf("original receive boundary not genuine")
			}
			if err := os.WriteFile(input.Ready, []byte("original native+Ordering durable; Member old"), 0600); err != nil {
				return err
			}
			for {
				time.Sleep(100 * time.Millisecond)
			}
		}
		return r.publisher.PublishReplica(progress)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.prepareSelectedTransport(logger); err != nil {
		t.Fatal(err)
	}
	t.Fatal("child transport returned before actual SIGKILL", f.serveSelectedTransport(ctx))
}

// A SchemaBoundary has no native semantic delta. Preserve the actual selected
// startup lease and issued Metadata facade, without allocating unrelated HTTP
// accounting or the 32-frame native business receive pool.
func prepareHAReceivedStructuralTransport(ctx context.Context, f *haSelectedRuntimeFoundation) error {
	if err := f.prepareSelectedRoots(ctx); err != nil {
		return err
	}
	if err := f.prepareHAPhysicalDerivedVector(ctx); err != nil {
		return err
	}
	return f.reopened.Owners.Components.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		var err error
		f.metadataStore, err = f.reopened.OpenHAMetadataStore(ctx)
		return err
	})
}

func publishHAReceivedStructuralCommon(ctx context.Context, f *haSelectedRuntimeFoundation) error {
	c := f.reopened.Owners.Components
	base := f.reopened.Seal
	base.Revision++
	if err := c.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		publication, err := statecut.PublishHASelectedMaintenanceCapturedStartupCaches(ctx, f.reopened.Owners, base, func(ctx context.Context) (statecut.StartupCacheCandidates, error) {
			return f.prepareHAPhysicalCheckpointCaches(ctx, f.reopened.Seal)
		})
		if err != nil {
			return err
		}
		f.reopened.Seal = publication.Seal
		return publication.CleanupError()
	}); err != nil {
		return err
	}
	return f.retireHAPhysicalDerivedVectorAfterCommon(ctx)
}
