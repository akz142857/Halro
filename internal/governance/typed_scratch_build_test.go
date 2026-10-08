package governance

import (
	"bytes"
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/ownedfile"
	"os"
	"path/filepath"
	"testing"
)

func TestFixedScratchGovernanceRejectsForeignScratchAndChangedNative(t *testing.T) {
	for _, scenario := range []string{"foreign-scratch", "changed-native"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			key := bytes.Repeat([]byte{0x79}, 32)
			native, err := Open(filepath.Join(dir, "native.wal"), key)
			if err != nil {
				t.Fatal(err)
			}
			defer native.Close()
			if _, err := native.Append(ctx, projectionEvent(1)); err != nil {
				t.Fatal(err)
			}
			budget := governanceSealBudget(t)
			before := budget.Snapshot().Used
			o := AuthenticatedGovernanceOptions{Native: native, Identity: [32]byte{2}, RootKey: key, ProjectionPath: filepath.Join(dir, "scratch.db"), ProjectionMaxDiskBytes: 16 << 20, RootDirectory: filepath.Join(dir, "root"), RootMaxDiskBytes: 32 << 20, RootMaxAppendBytes: 8 << 20, Budget: budget}
			foreign := []byte("saved DB is never construction authority")
			var phase func(string)
			if scenario == "foreign-scratch" {
				if err := os.WriteFile(o.ProjectionPath, foreign, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				phase = func(name string) {
					if name == "source-verified" {
						file, err := os.OpenFile(native.path, os.O_WRONLY, 0)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := file.WriteAt([]byte{0xff}, 20); err != nil {
							t.Fatal(err)
						}
						if err := file.Close(); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			packed, _, err := buildFixedPackedTypedGovernanceProjection(ctx, o, phase)
			if err == nil || packed != nil {
				t.Fatal("accepted untrusted construction", packed, err)
			}
			if budget.Snapshot().Used != before {
				t.Fatal("failed scratch/candidate did not actually retire", budget.Snapshot())
			}
			if scenario == "foreign-scratch" {
				got, readErr := os.ReadFile(o.ProjectionPath)
				if readErr != nil || !bytes.Equal(got, foreign) {
					t.Fatal("foreign bytes changed", got, readErr)
				}
			} else if _, err := os.Lstat(o.ProjectionPath); !os.IsNotExist(err) {
				t.Fatal("failed owned scratch survived", err)
			}
		})
	}
}

func TestFixedScratchGovernanceRestoredActualInodeRetiresConstructionDebt(t *testing.T) {
	for _, namespace := range []string{"scratch", "candidate"} {
		t.Run(namespace, func(t *testing.T) {
			dir := t.TempDir()
			key := bytes.Repeat([]byte{0x7b}, 32)
			native, err := Open(filepath.Join(dir, "native.wal"), key)
			if err != nil {
				t.Fatal(err)
			}
			defer native.Close()
			if _, err := native.Append(context.Background(), projectionEvent(1)); err != nil {
				t.Fatal(err)
			}
			budget := governanceSealBudget(t)
			before := budget.Snapshot().Used
			o := AuthenticatedGovernanceOptions{Native: native, Identity: [32]byte{4}, RootKey: key, ProjectionPath: filepath.Join(dir, "scratch.db"), ProjectionMaxDiskBytes: 16 << 20, RootDirectory: filepath.Join(dir, "root"), RootMaxDiskBytes: 32 << 20, RootMaxAppendBytes: 8 << 20, Budget: budget}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := o.ProjectionPath
			if namespace == "candidate" {
				path = o.RootDirectory
			}
			moved := path + ".actual-owner"
			foreign := []byte("preserve foreign bytes")
			p, ref, err := buildFixedPackedTypedGovernanceProjection(ctx, o, func(phase string) {
				if phase != "before-publication" {
					return
				}
				if err := os.Rename(path, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, foreign, 0600); err != nil {
					t.Fatal(err)
				}
				cancel()
			})
			var debt *PackedGovernanceBuildRetirement
			if !errors.Is(err, ownedfile.ErrRetirement) || !errors.Is(err, ownedfile.ErrReplaced) || !errors.As(err, &debt) || p == nil || ref != (TypedGovernanceReference{}) {
				t.Fatal("unresolved actual owner was discarded", p, ref, err)
			}
			charged := budget.Snapshot().Used
			if charged.Disk <= before.Disk || charged.RAM <= before.RAM {
				t.Fatal("unresolved derivative refunded construction", before, charged)
			}
			if err := debt.Retire(); err == nil || budget.Snapshot().Used != charged {
				t.Fatal("repeat failed retire lost fees", err, budget.Snapshot())
			}
			got, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(got, foreign) {
				t.Fatal("foreign bytes deleted", got, readErr)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(moved, path); err != nil {
				t.Fatal(err)
			}
			if err := debt.Retire(); err != nil || budget.Snapshot().Used != before {
				t.Fatal("restored actual inode did not retire/refund", err, budget.Snapshot())
			}
			if err := debt.Retire(); err != nil || budget.Snapshot().Used != before {
				t.Fatal("successful retirement not idempotent", err, budget.Snapshot())
			}
		})
	}
}
