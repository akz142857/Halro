package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestHASelectedReceiveRestartUsesActualSevenRootsWithoutChangingProgress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture := prepareHAMetadataMaintenanceFixture(t)
	prepareHAMetadataMaintenanceArtifacts(t, fixture)
	completeHAMemoryActivationBoundary(t, fixture)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := fixture.configs[1]
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
	stateBefore, err := os.ReadFile(cfg.ReplicationStatePath())
	if err != nil {
		t.Fatal(err)
	}
	orderBefore, err := os.ReadFile(cfg.OrderingJournalPath())
	if err != nil {
		t.Fatal(err)
	}
	path := cfg.ReplicationStatePath() + ".received.json"
	if err = os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	guard, err = lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	member, err := restartHASelectedReceiveReceiptSampled(ctx, cfg, fixture.keys.Master, guard, functionalHAMetadataManager(t, cfg), logger)
	if err = errors.Join(err, guard.Close()); err != nil {
		t.Fatal(err)
	}
	if member.Role != replication.RoleReplica || member.AppliedIndex != member.ConfirmedIndex || member.DurableIndex != member.AppliedIndex {
		t.Fatal(member)
	}
	actual, err := os.ReadFile(cfg.ReplicationStatePath())
	if err != nil || !bytes.Equal(actual, stateBefore) {
		t.Fatal("offline receipt retry changed actual Member", err)
	}
	actual, err = os.ReadFile(cfg.OrderingJournalPath())
	if err != nil || !bytes.Equal(actual, orderBefore) {
		t.Fatal("offline retry changed Ordering", err)
	}
	if actual, err = os.ReadFile(path + ".unqualified-0"); err != nil || len(actual) != 0 {
		t.Fatal("original incomplete candidate was deleted/reinterpreted", len(actual), err)
	}
	// The actual ordinary factory consumes its independently authenticated root
	// and newly born bank. No literal Member update or service-ready bit was set.
	guard, err = lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	f, err := openHASelectedRuntimeFoundation(ctx, cfg, fixture.keys.Master, guard, functionalHAMetadataManager(t, cfg), logger)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.prepareSelectedServices(ctx); err != nil {
		t.Fatal(err)
	}
	if err = f.prepareSelectedTransport(logger); err != nil {
		t.Fatal("genuine target transport could not reuse bank", err)
	}
	if err = errors.Join(f.Close(), guard.Close()); err != nil {
		t.Fatal(err)
	}
}
