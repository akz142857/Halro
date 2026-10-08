package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	bbolt "go.etcd.io/bbolt"
)

func providerBackupFixture(t *testing.T, n int) (*Store, uint64) {
	t.Helper()
	s, err := openForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.update(func(tx *Tx) error {
		b := tx.Bucket(bucketProviderResources)
		for k := 0; k < n; k++ {
			id := fmt.Sprintf("resource-%04d", k)
			raw, err := json.Marshal(map[string]any{"id": id, "object_path": "answer.sealed", "input_object_path": "request.sealed", "unused": strings.Repeat("x", 1024)})
			if err != nil {
				return err
			}
			if err := b.Put([]byte(id), raw); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	info, err := s.Info()
	if err != nil {
		t.Fatal(err)
	}
	return s, info.TxID
}

func TestProviderBackupWalkPagesAndReleasesReaders(t *testing.T) {
	s, txID := providerBackupFixture(t, 130)
	seen := 0
	if err := s.WalkProviderObjectReferences(context.Background(), txID, func(r ProviderObjectReference) error {
		if r.ID != fmt.Sprintf("resource-%04d", seen) || r.ObjectPath != "answer.sealed" || r.InputObjectPath != "request.sealed" {
			t.Fatal(r, seen)
		}
		if readers := s.db.Stats().OpenTxN; readers != 0 {
			t.Fatalf("callback holds %d read transactions", readers)
		}
		seen++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if seen != 130 {
		t.Fatal(seen)
	}
}

func TestProviderBackupWalkSnapshotFenceAndCancel(t *testing.T) {
	for _, kind := range []string{"first", "last", "cancel", "identity", "wide"} {
		t.Run(kind, func(t *testing.T) {
			s, txID := providerBackupFixture(t, 130)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "identity" || kind == "wide" {
				var err error
				if kind == "identity" {
					err = s.update(func(tx *Tx) error {
						return tx.Bucket(bucketProviderResources).Put([]byte("resource-0000"), []byte(`{"id":"different"}`))
					})
				} else {
					// Explicit malformed derived DB fixture: the native writer itself
					// refuses a row beyond its payload cap. Exercise pre-copy rejection.
					err = s.db.Update(func(tx *bbolt.Tx) error {
						return tx.Bucket(bucketProviderResources).Put([]byte("resource-0000"), []byte(strings.Repeat("x", pricingRecoveryWideRowBytes+1)))
					})
				}
				if err != nil {
					t.Fatal(err)
				}
				info, err := s.Info()
				if err != nil {
					t.Fatal(err)
				}
				txID = info.TxID
			}
			seen := 0
			err := s.WalkProviderObjectReferences(ctx, txID, func(ProviderObjectReference) error {
				seen++
				if kind == "cancel" {
					cancel()
				}
				if kind == "first" && seen == 1 || kind == "last" && seen == 130 {
					// Mutation succeeds because callback owns no reader. The next/final
					// fence must reject even an insertion behind the cursor.
					return s.update(func(tx *Tx) error {
						return tx.Bucket(bucketProviderResources).Put([]byte("behind"), []byte(`{"id":"behind"}`))
					})
				}
				return nil
			})
			if err == nil {
				t.Fatal("invalid scan accepted")
			}
			if (kind == "first" || kind == "last") && !errors.Is(err, ErrProviderBackupChanged) {
				t.Fatal(err)
			}
			if kind == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if kind == "wide" && !errors.Is(err, ErrPricingRecoveryLimit) {
				t.Fatal(err)
			}
		})
	}
}
