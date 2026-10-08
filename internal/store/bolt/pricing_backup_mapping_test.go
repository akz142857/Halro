package bolt

import (
	"context"
	"crypto/sha256"
	"errors"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"os"
	"path/filepath"
	"testing"

	bbolt "go.etcd.io/bbolt"
)

func TestLegacyPricingBackupMappingIsReadOnlyBoundedAndRetires(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	old, err := LegacyPricingBackupState(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	budget, _ := metadataMappingTestBudget(t, 1<<20)
	got, err := LegacyPricingBackupStateWithMappingBudget(context.Background(), path, 1<<20, budget)
	if err != nil || got != old {
		t.Fatal("bounded legacy hash changed compatibility", got, old, err)
	}
	if s := budget.Snapshot(); s.Used != (resourcebudget.Cost{}) {
		t.Fatal("raw reader escaped actual close", s)
	}
	got, err = LegacyPricingBackupStateWithMappingBudget(context.Background(), path, int64(len(before))-1, budget)
	if !errors.Is(err, ErrMetadataStoreDiskLimit) || got != (PricingBackupState{}) {
		t.Fatal("oversized raw DB was mapped", got, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	got, err = LegacyPricingBackupStateWithMappingBudget(canceled, path, 1<<20, budget)
	if !errors.Is(err, context.Canceled) || got != (PricingBackupState{}) {
		t.Fatal(got, err)
	}
	if s := budget.Snapshot(); s.Used != (resourcebudget.Cost{}) {
		t.Fatal("raw refusal retained mapping", s)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("legacy inspection changed source", err)
	}
}

func TestLegacyPricingHashDepthAndWideIntentAreBounded(t *testing.T) {
	for _, fault := range []string{"depth", "wide-intent"} {
		t.Run(fault, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "metadata.db")
			db, err := bbolt.Open(path, 0600, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = db.Update(func(tx *bbolt.Tx) error {
				if fault == "wide-intent" {
					b, err := tx.CreateBucket(bucketPricingAuditIntents)
					if err != nil {
						return err
					}
					return b.Put([]byte("wide"), make([]byte, pricingRecoveryWideRowBytes+1))
				}
				b, err := tx.CreateBucket(bucketDeploymentPriceVersions)
				if err != nil {
					return err
				}
				for i := 0; i < 32; i++ {
					b, err = b.CreateBucket([]byte("nested"))
					if err != nil {
						return err
					}
				}
				return b.Put([]byte("leaf"), []byte("value"))
			})
			if err := errors.Join(err, db.Close()); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			budget, _ := metadataMappingTestBudget(t, 32<<20)
			got, err := LegacyPricingBackupStateWithMappingBudget(context.Background(), path, 32<<20, budget)
			if !errors.Is(err, ErrPricingRecoveryLimit) || got != (PricingBackupState{}) {
				t.Fatal("oversized legacy graph was accepted", got, err)
			}
			if s := budget.Snapshot(); s.Used != (resourcebudget.Cost{}) {
				t.Fatal("rejected legacy scan retained mapping", s)
			}
			after, err := os.ReadFile(path)
			if err != nil || sha256.Sum256(after) != sha256.Sum256(before) {
				t.Fatal("rejection changed source bytes", err)
			}
		})
	}
}
