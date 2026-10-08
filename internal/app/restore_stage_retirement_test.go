package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestRestoreStageActualCloseDebtPreservesCandidateAndOriginalFees(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned-file", true: "native-ledger"}[native], func(t *testing.T) {
			cfg := enableBackupReferenceBudget(testConfig(t))
			ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx, finish, err := restoreReportContext(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			parent := t.TempDir()
			stage, err := os.MkdirTemp(parent, "stage-*")
			if err != nil {
				t.Fatal(err)
			}
			identity, err := os.Lstat(stage)
			if err != nil {
				t.Fatal(err)
			}
			parentIdentity, err := os.Lstat(parent)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(stage, "actual.wal")
			file, _, err := ownedfile.Create(path, os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString("candidate"); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			marker := ownedfile.ErrRetirement
			if native {
				marker = ledger.ErrSegmentRetirement
			}
			cause := errors.Join(marker, file.Close())
			before := pressure.logicalBudget().Snapshot().Used
			if err := retireRestoreStage(ctx, stage, identity, parentIdentity, cause); !errors.Is(err, errRestoreReportRetirement) {
				t.Fatal(err)
			}
			if err := restoreReportReady(ctx, nil); !errors.Is(err, errRestoreReportRetirement) {
				t.Fatal("publication accepted unknown actual close", err)
			}
			if err := finish(); !errors.Is(err, errRestoreReportRetirement) {
				t.Fatal(err)
			}
			if got := pressure.logicalBudget().Snapshot().Used; got != before {
				t.Fatal("original job refunded", before, got)
			}
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) != "candidate" {
				t.Fatal(string(raw), err)
			}
		})
	}
}

func TestRestoreStageReplacementPreservesBothDirectoryTrees(t *testing.T) {
	ctx, finish, err := restoreReportContext(context.Background(), enableBackupReferenceBudget(testConfig(t)))
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	stage, err := os.MkdirTemp(parent, "stage-*")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(stage)
	if err != nil {
		t.Fatal(err)
	}
	parentIdentity, err := os.Lstat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stage, stage+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stage, "foreign")
	if err := os.WriteFile(path, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := retireRestoreStage(ctx, stage, identity, parentIdentity, nil); !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal(err)
	}
	if err := finish(); !errors.Is(err, errRestoreReportRetirement) {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "preserve" {
		t.Fatal(string(raw), err)
	}
	if _, err := os.Stat(stage + ".original"); err != nil {
		t.Fatal(err)
	}
}
