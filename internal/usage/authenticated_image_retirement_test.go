package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestAuthenticatedUsageActualImageCloseDebtKeepsSameManagerFees(t *testing.T) {
	f := newUsageAdvanceFixture(t)
	baseline := f.budget.Snapshot().Used
	credit, err := f.budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "scratch.db")
	var actual *os.File
	p, err := buildAuthenticatedUsageProjection(context.Background(), AuthenticatedUsageOptions{Native: f.native, Owner: f.owner, Identity: sha256.Sum256([]byte("retirement-usage")), UsageKey: bytes.Repeat([]byte{0x91}, 32), ScratchPath: path, RootDirectory: filepath.Join(dir, "root"), ScratchMaxDiskBytes: 16 << 20, RootMaxDiskBytes: 32 << 20, RootMaxAppendBytes: 4 << 20, ScratchReservation: credit, Budget: f.budget}, func(file *os.File) error {
		actual = file
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return file.Close()
	})
	if p == nil || !errors.Is(err, ownedfile.ErrRetirement) || p.retiredImage != actual || actual == nil || p.retiredScratch == nil {
		t.Fatal(p, err)
	}
	if err := credit.Release(); err != nil {
		t.Fatal(err)
	}
	before := f.budget.Snapshot().Used
	if before.RAM < baseline.RAM+usageSealWorkspaceRAMBytes || before.Disk < baseline.Disk+16<<20 {
		t.Fatal("caller release refunded uncertain image", baseline, before)
	}
	for range 3 {
		if !errors.Is(p.Close(), ownedfile.ErrRetirement) || !errors.Is(p.Discard(), ownedfile.ErrRetirement) || f.budget.Snapshot().Used != before {
			t.Fatal("repeat cleanup erased debt")
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("uncertain actual image lost", err)
	}
}
