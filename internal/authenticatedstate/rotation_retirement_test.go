package authenticatedstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestRotationRootRetirementRequiresActualSelectedSuccessorAndCapturedNamespace(t *testing.T) {
	for _, mode := range []string{"success", "foreign", "unselected", "different-manager", "reader-close"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			o := storeOptions(t)
			o.MaxDiskBytes = 64 << 10
			o.MaxAppendBytes = 16 << 10
			create := func(path, value string, options StoreOptions) *Store {
				sent := false
				s, err := CreateStore(ctx, path, options, func() (Row, bool, error) {
					if sent {
						return Row{}, false, nil
					}
					sent = true
					return Row{Key: []byte("credential"), Value: []byte(value)}, true, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			creator := create(filepath.Join(dir, "old"), "old cipher", o)
			schema := SealRootSchema{Kind: SealRootState, Component: SealComponentMetadata, IndexID: 1, Codec: 1}
			old, err := creator.SealRootReference(ctx, schema)
			if err != nil {
				t.Fatal(err)
			}
			root, err := creator.RootIdentity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := creator.Close(); err != nil {
				t.Fatal(err)
			}
			credit, err := creator.DiskReservation()
			if err != nil {
				t.Fatal(err)
			}
			o.DiskReservation = credit
			o.ExpectedRoot = &root
			source, err := OpenStore(ctx, creator.dir, o)
			if err != nil {
				t.Fatal(err)
			}
			nextOptions := o
			nextOptions.DiskReservation = resourcebudget.Permit{}
			nextOptions.ExpectedRoot = nil
			if mode == "different-manager" {
				nextOptions.Budget = testBudget(t)
			}
			next := create(filepath.Join(dir, "new"), "new cipher", nextOptions)
			defer next.Close()
			newRoot, err := next.SealRootReference(ctx, schema)
			if err != nil {
				t.Fatal(err)
			}
			guard, err := lock.Acquire(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			seal := sealCandidateFixture()
			seal.Roots[0] = old
			p, err := OpenSealCandidatePublisher(dir, guard, o.Key, sealHardFor(seal), o.Budget, sealPublisherReservation(t, o.Budget))
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if _, err := p.Publish(ctx, seal); err != nil {
				t.Fatal(err)
			}
			if mode != "unselected" {
				seal.Revision++
				seal.Roots[0] = newRoot
				if _, err := p.Publish(ctx, seal); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "foreign" {
				if err := os.WriteFile(filepath.Join(source.dir, "foreign.data"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := source.CaptureRotationRetirementLock(); err != nil {
				t.Fatal(err)
			}
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			if mode == "reader-close" {
				before := o.Budget.Snapshot()
				err = source.retireClosedRotationRoot(ctx, next, old, p, guard, sealEligibilityFor(seal), credit, func(path string) (*os.File, error) {
					f, err := os.Open(path)
					if err != nil {
						return nil, err
					}
					if err := f.Close(); err != nil {
						return nil, err
					}
					return f, nil
				})
				var debt *RotationRootRetirementFailure
				if !errors.Is(err, ownedfile.ErrRetirement) || !errors.As(err, &debt) || debt.reader == nil || debt.source != source || debt.successor != next {
					t.Fatal("lost actual failed-close reader owner", err)
				}
				if after := o.Budget.Snapshot(); after.NormalClaims+after.RecoveryClaims <= before.NormalClaims+before.RecoveryClaims {
					t.Fatal("failed reader lost work claim", before, after)
				}
				if !errors.Is(source.Close(), ownedfile.ErrRetirement) {
					t.Fatal("reader-close debt was not sticky")
				}
			} else {
				err = source.RetireClosedRotationRoot(ctx, next, old, p, guard, sealEligibilityFor(seal), credit)
			}
			if mode == "success" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(source.dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("captured old pack survived", err)
				}
			} else {
				if err == nil {
					t.Fatal("retired an unauthorized namespace")
				}
				if _, err := os.Lstat(filepath.Join(source.dir, "pages")); err != nil {
					t.Fatal("removed old pack before proof", err)
				}
				if mode == "foreign" && !errors.Is(err, ownedfile.ErrReplaced) {
					t.Fatal(err)
				}
			}
		})
	}
}
