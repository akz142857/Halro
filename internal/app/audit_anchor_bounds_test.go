package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func TestAuditWitnessBoundsBeforeReadGraphAndNativeRepair(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "witness")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(anchorFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if anchors, err := LoadAuditAnchorsFile(path); !errors.Is(err, errAnchorLimit) || anchors != nil {
		t.Fatal(anchors, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAuditAnchorsFile(path); !errors.Is(err, errAnchorLimit) {
		t.Fatal("followed symlink", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("{}\n", anchorMaximumRows+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if anchors, err := LoadAuditAnchorsFile(path); !errors.Is(err, errAnchorLimit) || anchors != nil {
		t.Fatal(len(anchors), err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("{}\n", 2)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+rotatedAnchorSuffix, []byte(strings.Repeat("{}\n", anchorMaximumRows-1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAuditAnchorsFile(path); !errors.Is(err, errAnchorLimit) {
		t.Fatal("separate generations bypassed total graph bound", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := loadAuditAnchorsFileContext(ctx, "missing"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if result, err := VerifyAuditAnchors(context.Background(), testConfig(t), make([]boltstore.AuditAnchor, anchorMaximumRows+1)); !errors.Is(err, errAnchorLimit) || result != nil {
		t.Fatal("grew maps before graph bound", err)
	}
	cfg := budgetedBackupOperationFixture(t)
	raw, err := os.ReadFile(cfg.AuditPath())
	if err != nil {
		t.Fatal(err)
	}
	partial := append(raw, 0x05)
	if err := os.WriteFile(cfg.AuditPath(), partial, 0600); err != nil {
		t.Fatal(err)
	}
	if result, err := VerifyAuditAnchors(context.Background(), cfg, nil); err == nil || result != nil {
		t.Fatal("approved a torn authority", err)
	}
	if after, err := os.ReadFile(cfg.AuditPath()); err != nil || !bytes.Equal(partial, after) {
		t.Fatal("repaired source", err)
	}
}

type anchorBlockingWriter struct {
	entered chan struct{}
	finish  chan struct{}
}

func (w *anchorBlockingWriter) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.finish
	return len(p), nil
}

func TestAuditWitnessOriginalSharedRootAndActualOutputJoin(t *testing.T) {
	cfg := budgetedBackupOperationFixture(t)
	cfg.ResourceBudget.AuditLogDiskBytes = 8 << 20
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	occupied, err := pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{RAM: cfg.ResourceBudget.RecoveryRAMBytes - resourcebudget.ClaimRAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteVerifyAuditAnchorsFile(ctx, cfg, "missing", io.Discard); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("witness created a fresh pool or read source before fees", err)
	}
	if err := occupied.Release(); err != nil {
		t.Fatal(err)
	}
	summary, err := VerifyAudit(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "witness")
	encoded, err := json.Marshal(boltstore.AuditAnchor{Sequence: 1, Records: summary.Records, LastHash: summary.LastHash, InstanceID: "witness"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	writer := &anchorBlockingWriter{make(chan struct{}), make(chan struct{})}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- WriteVerifyAuditAnchorsFile(ctx, cfg, path, writer) }()
	select {
	case <-writer.entered:
	case err := <-done:
		t.Fatal("failed before output", err)
	}
	if s := pressure.logicalBudget().Snapshot(); s.Classes[resourcebudget.Recovery].RAM < anchorWorkRAMBytes || s.Classes[resourcebudget.Recovery].Disk < cfg.ResourceBudget.AuditLogDiskBytes {
		t.Fatal("actual output escaped shared owner", s)
	}
	cancel()
	select {
	case err := <-done:
		t.Fatal("cancellation returned before actual output", err)
	default:
	}
	if s := pressure.logicalBudget().Snapshot(); s.Classes[resourcebudget.Recovery].RAM < anchorWorkRAMBytes {
		t.Fatal("cancellation refunded live output", s)
	}
	close(writer.finish)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if s := pressure.logicalBudget().Snapshot(); s.Classes[resourcebudget.Recovery].RAM >= auditInspectionRAMBytes || s.Classes[resourcebudget.Recovery].Disk < cfg.ResourceBudget.AuditLogDiskBytes {
		t.Fatal("closed RAM or surviving native file fee incorrect", s)
	}
}
