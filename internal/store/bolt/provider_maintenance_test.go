package bolt

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

func TestProviderMaintenancePagesAllowCleanupAndKeepFiniteUpperKey(t *testing.T) {
	s, _ := providerBackupFixture(t, 131)
	seen := 0
	err := s.WalkProviderResourcesMaintenance(context.Background(), func(resource domain.ProviderResource) error {
		if resource.ID != fmt.Sprintf("resource-%04d", seen) || s.db.Stats().OpenTxN != 0 {
			t.Fatal("callback order or reader lifetime", resource.ID, seen, s.db.Stats())
		}
		seen++
		// Remove the row and append traffic beyond the pass's original upper
		// key. The caller must neither deadlock nor chase newly appended rows.
		return s.update(func(tx *Tx) error {
			bucket := tx.Bucket(bucketProviderResources)
			if err := bucket.Delete([]byte(resource.ID)); err != nil {
				return err
			}
			id := fmt.Sprintf("zz-new-%04d", seen)
			return bucket.Put([]byte(id), []byte(fmt.Sprintf(`{"id":%q}`, id)))
		})
	})
	if err != nil || seen != 131 {
		t.Fatal(seen, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	seen = 0
	err = s.WalkProviderResourcesMaintenance(canceled, func(domain.ProviderResource) error { seen++; cancel(); return nil })
	if !errors.Is(err, context.Canceled) || seen != 1 {
		t.Fatal("cancel ignored", seen, err)
	}
}

func TestProviderReferenceSnapshotExcludesWritersWithoutChangingNativeFacts(t *testing.T) {
	s, txID := providerBackupFixture(t, 131)
	if err := s.WithProviderObjectReferenceSnapshot(context.Background(), txID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	info, err := s.Info()
	if err != nil || info.TxID != txID {
		t.Fatal("reference gate changed Metadata", info, err)
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.WithProviderObjectReferenceSnapshot(context.Background(), txID, func() error { close(entered); <-resume; return nil })
	}()
	<-entered
	writer := make(chan error, 1)
	go func() {
		writer <- s.update(func(tx *Tx) error {
			return tx.Bucket(bucketProviderResources).Put([]byte("new-reference"), []byte(`{"id":"new-reference","object_path":"previous-orphan"}`))
		})
	}()
	select {
	case err := <-writer:
		close(resume)
		<-done
		t.Fatal("writer crossed deletion authority", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
	called := false
	err = s.WithProviderObjectReferenceSnapshot(context.Background(), txID, func() error { called = true; return nil })
	if !errors.Is(err, ErrProviderBackupChanged) || called {
		t.Fatal("stale absence licensed deletion", called, err)
	}
}
