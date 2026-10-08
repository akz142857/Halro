package governance

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

func TestAuthenticatedGovernanceActualImageExitPreservesOriginalFees(t *testing.T) {
	for _, fault := range []string{"actual close", "replaced name"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			native, err := Open(filepath.Join(dir, "governance.wal"), bytes.Repeat([]byte{0x52}, 32))
			if err != nil {
				t.Fatal(err)
			}
			defer native.Close()
			if _, err := native.Append(ctx, projectionEvent(21)); err != nil {
				t.Fatal(err)
			}
			budget := governanceSealBudget(t)
			credit, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 16 << 20})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "scratch.db")
			var actual *os.File
			p, err := buildAuthenticatedGovernanceProjection(ctx, AuthenticatedGovernanceOptions{Native: native, Identity: [32]byte{1}, RootKey: bytes.Repeat([]byte{0x83}, 32), ProjectionPath: path, RootDirectory: filepath.Join(dir, "root"), ProjectionMaxDiskBytes: 16 << 20, RootMaxDiskBytes: 32 << 20, RootMaxAppendBytes: 4 << 20, ProjectionDiskReservation: credit, Budget: budget}, func(file *os.File) error {
				actual = file
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				if fault == "actual close" {
					return file.Close()
				}
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				return os.WriteFile(path, []byte("foreign"), 0600)
			})
			if p == nil || !errors.Is(err, ownedfile.ErrRetirement) || p.retiredScratch == nil || actual == nil {
				t.Fatal("unknown physical image exit lost cleanup owner", p, err)
			}
			if err := credit.Release(); err != nil {
				t.Fatal(err)
			}
			before := budget.Snapshot().Used
			if before.RAM < governanceSealWorkspaceRAMBytes || before.Disk < 16<<20 {
				t.Fatal("caller release refunded actual source image", before)
			}
			for range 3 {
				if !errors.Is(p.Close(), ownedfile.ErrRetirement) || !errors.Is(p.Discard(), ownedfile.ErrRetirement) || budget.Snapshot().Used != before {
					t.Fatal("cleanup erased retained inode/fees")
				}
			}
			if fault == "actual close" {
				if p.retiredImage != actual {
					t.Fatal("actual descriptor reference lost")
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatal(err)
				}
			} else {
				if !errors.Is(err, ownedfile.ErrReplaced) {
					t.Fatal(err)
				}
				if got, err := os.ReadFile(path); err != nil || string(got) != "foreign" {
					t.Fatal(string(got), err)
				}
				if _, err := os.Stat(path + ".original"); err != nil {
					t.Fatal("original image was not retired", err)
				}
			}
		})
	}
}
