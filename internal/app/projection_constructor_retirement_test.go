package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestRuntimeProjectionConstructorCloseDebtKeepsFeesDespiteMissingName(t *testing.T) {
	for _, kind := range []string{"accounting", "governance", "usage", "replica"} {
		t.Run(kind, func(t *testing.T) {
			cfg := enableBackupReferenceBudget(testConfig(t))
			ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			permit, err := acquireLedgerProjectionDisk(ctx, cfg, pressure)
			if err != nil || permit == nil {
				t.Fatal(permit, err)
			}
			path := filepath.Join(t.TempDir(), "candidate.db")
			file, _, err := ownedfile.Create(path, os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			cause := errors.Join(ownedfile.ErrRetirement, file.Close())
			if err := os.Rename(path, path+".escaped"); err != nil {
				t.Fatal(err)
			}
			var closeOwner func() error
			switch kind {
			case "accounting":
				o := &runtimeAccountingState{path: path, diskPermit: permit, retirementErr: cause}
				closeOwner = o.close
			case "governance":
				o := &runtimeGovernanceOwner{path: path, diskPermit: permit, retirementErr: cause}
				closeOwner = o.close
			case "usage":
				o := &runtimeUsageOwner{path: path, diskPermit: permit, retirementErr: cause}
				closeOwner = o.close
			case "replica":
				o := &replicaAccountingGeneration{output: path, diskPermit: permit, retirementErr: cause}
				closeOwner = o.Close
			}
			before := pressure.logicalBudget().Snapshot().Used
			for n := 0; n < 2; n++ {
				if err := closeOwner(); !errors.Is(err, ownedfile.ErrRetirement) {
					t.Fatal("actual constructor close debt disappeared", err)
				}
				if got := pressure.logicalBudget().Snapshot().Used; got != before {
					t.Fatal("missing name refunded actual unknown constructor", before, got)
				}
			}
			if _, err := os.Stat(path + ".escaped"); err != nil {
				t.Fatal("escaped original inode removed", err)
			}
		})
	}
}

func TestIndependentScratchRetirementCannotRefundConstructorDebt(t *testing.T) {
	cfg := enableBackupReferenceBudget(testConfig(t))
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	credit, err := acquireBackupProjectionBudget(ctx, cfg, backupLedgerProjection)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "candidate")
	file, _, err := ownedfile.Create(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	cause := errors.Join(ownedfile.ErrRetirement, file.Close())
	before := pressure.logicalBudget().Snapshot().Used
	if err := credit.retire(withRetirementDebt(nil, cause)); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal("independent scratch repaid unknown engine", err)
	}
	if got := pressure.logicalBudget().Snapshot().Used; got != before {
		t.Fatal("constructor debt repaid by unrelated cleanup", before, got)
	}
}
