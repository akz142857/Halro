package governance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	bbolt "go.etcd.io/bbolt"
)

func TestPackedTypedGovernanceMatchesOrdinaryReducerAtFixed64MiB(t *testing.T) {
	for _, count := range []int{0, 1000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			key := bytes.Repeat([]byte{0x78}, 32)
			native, err := Open(filepath.Join(dir, "native.wal"), key)
			if err != nil {
				t.Fatal(err)
			}
			defer native.Close()
			for i := 0; i < count; i++ {
				if _, err := native.Append(ctx, projectionEvent(i)); err != nil {
					t.Fatal(err)
				}
			}
			budget := governanceSealBudget(t)
			before := budget.Snapshot().Used
			o := AuthenticatedGovernanceOptions{Native: native, Identity: [32]byte{1}, RootKey: key, ProjectionPath: filepath.Join(dir, "scratch.db"), ProjectionMaxDiskBytes: 32 << 20, RootDirectory: filepath.Join(dir, "root"), RootMaxDiskBytes: 64 << 20, RootMaxAppendBytes: 8 << 20, Budget: budget}
			packed, ref, err := BuildPackedTypedGovernanceProjection(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer packed.Discard()
			identity, err := packed.cow.RootIdentity(ctx)
			if err != nil || identity.Revision != 1 || ref.Projection.Root.State.Extent >= 64<<20 || ref.Projection.Schema.Codec != TypedGovernanceSealCodec {
				t.Fatal("initial pack did not preserve typed codec/cut or accumulated rewrites", identity, ref, err)
			}
			if _, err := os.Lstat(o.ProjectionPath); !os.IsNotExist(err) {
				t.Fatal("private scratch survived", err)
			}
			if packed.db != nil || packed.file != nil {
				t.Fatal("ordinary reducer retained")
			}
			ordinary, err := BuildGovernanceProjectionWithLimit(ctx, native.path, key, filepath.Join(dir, "ordinary.db"), native.Summary(), 32<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer ordinary.Discard()
			var rows uint64
			err = packed.viewRows(ctx, func(typed any, head Summary) error {
				if head != native.Summary() {
					return ErrCorrupt
				}
				return ordinary.db.View(func(tx *bbolt.Tx) error {
					for _, name := range governanceProjectionBuckets {
						if err := tx.Bucket([]byte(name)).ForEach(func(k, want []byte) error {
							got, found, err := govRawGet(typed, name, k)
							if err != nil || !found || !bytes.Equal(got, want) {
								return fmt.Errorf("row %s/%x differs: %w", name, k, errors.Join(ErrCorrupt, err))
							}
							rows++
							return nil
						}); err != nil {
							return err
						}
					}
					return nil
				})
			})
			if err != nil || identity.Rows != rows+1 {
				t.Fatal("whole namespace differs", rows, identity, err)
			}
			if err := packed.Discard(); err != nil {
				t.Fatal(err)
			}
			if after := budget.Snapshot().Used; after != before {
				t.Fatal("construction fees survived actual retirement", before, after)
			}
			t.Logf("outcomes=%d initial pack extent=%d rows=%d revision=%d", count, ref.Projection.Root.State.Extent, identity.Rows, identity.Revision)
		})
	}
}

func TestPackedTypedGovernanceRejectsForeignScratchAndChangedNative(t *testing.T) {
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
			packed, _, err := buildPackedTypedGovernanceProjection(ctx, o, phase)
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

func TestPackedTypedGovernanceRetainsFeesAfterSourceRetirementFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	key := bytes.Repeat([]byte{0x7a}, 32)
	native, err := Open(filepath.Join(dir, "native.wal"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	if _, err := native.Append(ctx, projectionEvent(1)); err != nil {
		t.Fatal(err)
	}
	native.closeReaderFile = func(*os.File) error { return errors.New("injected uncertain source close") }
	budget := governanceSealBudget(t)
	before := budget.Snapshot().Used
	o := AuthenticatedGovernanceOptions{Native: native, Identity: [32]byte{3}, RootKey: key, ProjectionPath: filepath.Join(dir, "scratch.db"), ProjectionMaxDiskBytes: 16 << 20, RootDirectory: filepath.Join(dir, "root"), RootMaxDiskBytes: 32 << 20, RootMaxAppendBytes: 8 << 20, Budget: budget}
	packed, ref, err := BuildPackedTypedGovernanceProjection(ctx, o)
	var debt *PackedGovernanceBuildRetirement
	if !errors.Is(err, ownedfile.ErrRetirement) || !errors.As(err, &debt) || packed == nil || ref != (TypedGovernanceReference{}) || budget.Snapshot().Used.RAM <= before.RAM {
		t.Fatal("unknown actual source close refunded construction or returned authority", packed, ref, err, budget.Snapshot())
	}
	charged := budget.Snapshot().Used
	if packed.Discard() == nil || budget.Snapshot().Used != charged {
		t.Fatal("repeat retirement erased actual debt")
	}
}

func TestPackedTypedGovernanceRestoredActualInodeRetiresConstructionDebt(t *testing.T) {
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
			p, ref, err := buildPackedTypedGovernanceProjection(ctx, o, func(phase string) {
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
