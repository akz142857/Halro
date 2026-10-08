package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestHAMetadataScratchFixedSlotsPreserveForeignBytesAndRetainCleanupDebt(t *testing.T) {
	f := prepareHAMetadataMaintenanceFixture(t)
	prepareHAMetadataMaintenanceArtifacts(t, f)
	cfg := f.configs[1]
	workspace := filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory)
	member, key, err := readMemberState(cfg, f.keys.Master)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key[:])
	_, authority, err := readHAMetadataCarrier(context.Background(), filepath.Join(workspace, "carrier.json"), key[:], member)
	if err != nil {
		t.Fatal(err)
	}
	manager := functionalHAMetadataManager(t, cfg)
	opaque := filepath.Join(workspace, "ledger-maintenance.0")
	if err := os.Mkdir(opaque, 0700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(opaque, "projection.db")
	old := []byte("unqualified foreign bytes")
	if err := os.WriteFile(foreign, old, 0600); err != nil {
		t.Fatal(err)
	}
	scratch, err := createHAMetadataScratch(context.Background(), workspace, key[:], authority, member, manager)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(scratch.path) != "ledger-maintenance.1" {
		t.Fatal("opaque old slot was reused", scratch.path)
	}
	if err := scratch.Discard(); err != nil {
		t.Fatal(err)
	}
	if manager.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("actual joined scratch fees leaked", manager.Snapshot())
	}
	scratch, err = createHAMetadataScratch(context.Background(), workspace, key[:], authority, member, manager)
	if err != nil {
		t.Fatal(err)
	}
	unexpected := filepath.Join(scratch.path, "projection.db")
	if err := os.WriteFile(unexpected, old, 0600); err != nil {
		t.Fatal(err)
	}
	if err := scratch.Discard(); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal("nonempty actual namespace refunded its fee", err)
	}
	if manager.Snapshot().Used.Disk != haMetadataScratchReceiptBytes {
		t.Fatal("unresolved cleanup debt lost admitted Disk", manager.Snapshot())
	}
	for _, path := range []string{foreign, unexpected} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, old) {
			t.Fatal("foreign bytes removed or modified", path, err)
		}
	}
	for _, name := range []string{"ledger-maintenance.2", "ledger-maintenance.3"} {
		if err := os.Mkdir(filepath.Join(workspace, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	before := manager.Snapshot().Used
	if _, err := createHAMetadataScratch(context.Background(), workspace, key[:], authority, member, manager); err == nil {
		t.Fatal("fifth scratch namespace admitted")
	}
	if manager.Snapshot().Used != before {
		t.Fatal("slot exhaustion leaked a new fee", manager.Snapshot(), before)
	}
	if _, err := os.Lstat(filepath.Join(workspace, "ledger-maintenance.4")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("slot count grew with retry", err)
	}
}
