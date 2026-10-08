package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/auditlimits"
	"github.com/akz142857/Halro/internal/metadatalimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestCommandMetadataSharesActualFilesMappingAndAudit(t *testing.T) {
	cfg := commandBudgetFixture(t)
	b := cfg.ResourceBudget
	b.MetadataStoreDiskBytes = 1 << 20
	b.RecoveryClaims++
	b.MaxClaims += 2
	b.ControlRAMBytes += 2 * resourcebudget.ClaimRAMBytes
	b.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes + 8*commandMetadataFileOwnerRAMBytes
	b.RecoveryRAMBytes += 8 * commandMetadataFileOwnerRAMBytes
	b.TotalRAMBytes += 2*resourcebudget.ClaimRAMBytes + 16*commandMetadataFileOwnerRAMBytes
	addMetadataMappingFixtureRAM(t, b)
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	manager := pressure.logicalBudget()
	ram, _ := metadatalimits.MappingRAMBytes(b.MetadataStoreDiskBytes)
	path := filepath.Join(t.TempDir(), "metadata.db")
	store, err := openCommandMetadataStore(ctx, cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	if s := manager.Snapshot(); s.RecoveryClaims != 2 || s.Classes[resourcebudget.Recovery].Disk != b.MetadataStoreDiskBytes || s.Classes[resourcebudget.Recovery].RAM != ram+commandMetadataFileOwnerRAMBytes {
		t.Fatal("metadata forked file or mapping ownership", s)
	}
	log, err := openOneShotConfiguredAuditLog(ctx, cfg, filepath.Join(t.TempDir(), "audit.log"), make([]byte, 32), audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(ctx, audit.Event{EventID: "aud-budget", OccurredAt: time.Now().UTC(), ActorType: "local_cli", Action: "metadata.budget.test", TargetType: "metadata", TargetID: "test", Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	if s := manager.Snapshot(); s.RecoveryClaims != 3 || s.Classes[resourcebudget.Recovery].RAM != ram+commandMetadataFileOwnerRAMBytes+auditlimits.MinCacheRAMBytes {
		t.Fatal("Audit forked the active Metadata manager", s)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openCommandMetadataReadOnly(ctx, cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	if s := manager.Snapshot(); s.RecoveryClaims != 2 || s.Classes[resourcebudget.Recovery].Disk != b.MetadataStoreDiskBytes {
		t.Fatal("sequential handle lost or duplicated persistent file admission", s)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < commandMetadataFileSlots; i++ {
		if _, err := reserveCommandMetadataFile(ctx, cfg, filepath.Join(t.TempDir(), "stage.db")); err != nil {
			t.Fatal(err)
		}
	}
	absent := filepath.Join(t.TempDir(), "absent", "metadata.db")
	if store, err := openCommandMetadataStore(ctx, cfg, absent); store != nil || err == nil {
		t.Fatal("fixed file slots grew", err)
	}
	if _, err := os.Stat(filepath.Dir(absent)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owner refusal created output", err)
	}
	if s := manager.Snapshot(); s.Classes[resourcebudget.Recovery].RAM != commandMetadataFileSlots*commandMetadataFileOwnerRAMBytes || s.Classes[resourcebudget.Recovery].Disk != commandMetadataFileSlots*b.MetadataStoreDiskBytes {
		t.Fatal("closed files were refunded or mapping was retained", s)
	}
}

func TestCommandMetadataRejectsDroppedScopeBeforeIO(t *testing.T) {
	cfg := commandBudgetFixture(t)
	b := cfg.ResourceBudget
	b.MetadataStoreDiskBytes = 1 << 20
	b.RecoveryClaims++
	b.MaxClaims += 2
	b.ControlRAMBytes += 2 * resourcebudget.ClaimRAMBytes
	b.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
	b.TotalRAMBytes += 2 * resourcebudget.ClaimRAMBytes
	addMetadataMappingFixtureRAM(t, b)
	path := filepath.Join(t.TempDir(), "absent", "metadata.db")
	if store, err := openCommandMetadataStore(context.Background(), cfg, path); store != nil || err == nil {
		t.Fatal("command silently substituted a manager", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refusal entered IO", err)
	}
}
