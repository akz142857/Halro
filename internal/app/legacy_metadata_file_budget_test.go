package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/metadatalimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func TestSelectedLegacyMetadataBootstrapUsesOriginalMappingOwnerAndActualFileCeiling(t *testing.T) {
	ctx := context.Background()
	b := runtimeRAMBudgetFixture()
	b.MemoryStateRequired, b.MetadataStoreDiskBytes, b.LegacyMetadataFileDiskBytes = true, 64<<20, 1<<20
	b.TotalDiskBytes, b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes = 256<<20, 128<<20, 128<<20
	p := runtimePressureFixture()
	cfg := config.Config{ResourceBudget: &b, ResourcePressure: &p}
	cfg.Storage.DataDir, cfg.Storage.MetadataFile = t.TempDir(), "metadata.db"
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	manager, err := resourcebudget.New(b.Limits(p.MaxAge.Value()))
	if err != nil {
		t.Fatal(err)
	}
	pressure := &resourcePressureRuntime{budget: manager}
	owner, err := acquireMetadataStoreDisk(ctx, cfg, pressure)
	if err != nil {
		t.Fatal(err)
	}
	ram, _ := metadatalimits.MappingRAMBytes(b.LegacyMetadataFileDiskBytes)
	if s := manager.Snapshot(); s.Used.Disk != b.MetadataStoreDiskBytes || s.Classes[resourcebudget.Recovery].RAM != ram {
		t.Fatal("original full Disk or bounded actual mapping charge lost", s)
	}
	bootstrap := runtimeMetadataBootstrap{pressure: pressure, owner: owner, budget: b, policy: p, path: cfg.MetadataPath()}
	bootCtx := context.WithValue(ctx, runtimeMetadataBootstrapKey{}, bootstrap)
	before := manager.Snapshot()
	store, err = openMasterKeyMetadata(bootCtx, cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if after := manager.Snapshot(); after.Used != before.Used || after.RecoveryClaims != before.RecoveryClaims {
		t.Fatal("bootstrap reminted its original mapping", before, after)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Actual-FD size rejection happens before any mmap and keeps the original
	// persistent reservation; the independent COW ceiling is not a waiver.
	if err := os.Truncate(cfg.MetadataPath(), 2<<20); err != nil {
		t.Fatal(err)
	}
	if next, err := openMasterKeyMetadata(bootCtx, cfg, true); next != nil || !errors.Is(err, boltstore.ErrMetadataStoreDiskLimit) {
		t.Fatal("oversized legacy file entered the selected mapping", err)
	}
	if after := manager.Snapshot(); after.Used != before.Used {
		t.Fatal("rejected handle lost original ownership", after)
	}
	if err := owner.retireRAMAfterClose(nil); err != nil {
		t.Fatal(err)
	}
	if next, err := openMasterKeyMetadata(bootCtx, cfg, true); next != nil || err == nil {
		t.Fatal("retired mapping credit reused")
	}
	if got := metadataLegacyFileCeiling(cfg); got != 1<<20 {
		t.Fatal(got)
	}
	if err := owner.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Storage.DataDir, "metadata.db")); err != nil {
		t.Fatal("file reservation granted deletion", err)
	}
}
