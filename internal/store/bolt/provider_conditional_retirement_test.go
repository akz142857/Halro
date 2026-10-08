package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

func providerConditionalFixture(t *testing.T) (*Store, domain.ProviderResource) {
	t.Helper()
	s, _ := providerBackupFixture(t, 0)
	r := domain.ProviderResource{ID: "legacy-object", ProjectID: "project", Kind: domain.ResourceFile, ObjectPath: "legacy.content", Status: "uploaded", CreationStatus: "completed", Revision: 1}
	r.IdempotencyKeyHash[0] = 1
	if err := s.update(func(tx *Tx) error {
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketProviderResources).Put([]byte(r.ID), raw); err != nil {
			return err
		}
		return tx.Bucket(bucketProviderResourceIdem).Put(providerResourceIdemKey(r.ProjectID, r.Kind, r.IdempotencyKeyHash), []byte(r.ID))
	}); err != nil {
		t.Fatal(err)
	}
	return s, r
}

func TestProviderConditionalRetirementRejectsChangedRowAndRollsBackFailure(t *testing.T) {
	for _, mode := range []string{"new revision", "same revision changed field", "retirement failure", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			s, expected := providerConditionalFixture(t)
			if mode == "new revision" || mode == "same revision changed field" {
				changed := expected
				changed.Status = "refreshed"
				if mode == "new revision" {
					changed.Revision++
				}
				if err := s.update(func(tx *Tx) error {
					raw, err := json.Marshal(changed)
					if err != nil {
						return err
					}
					return tx.Bucket(bucketProviderResources).Put([]byte(changed.ID), raw)
				}); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.Info()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			called := false
			failure := errors.New("physical retirement failed")
			err = s.DeleteProviderResourceIfUnchanged(ctx, expected, func() error { called = true; return failure })
			if mode == "retirement failure" {
				if !called || !errors.Is(err, failure) {
					t.Fatal(called, err)
				}
			} else if mode == "canceled" {
				if called || !errors.Is(err, context.Canceled) {
					t.Fatal(called, err)
				}
			} else if called || !errors.Is(err, ErrRevisionConflict) {
				t.Fatal(called, err)
			}
			after, err := s.Info()
			if err != nil || after.TxID != before.TxID {
				t.Fatal("rejected retirement changed Metadata", before, after, err)
			}
			if _, err := s.ProviderResource(context.Background(), expected.ProjectID, expected.ID); err != nil {
				t.Fatal("row removed", err)
			}
			if err := s.view(func(tx *Tx) error {
				if string(tx.Bucket(bucketProviderResourceIdem).Get(providerResourceIdemKey(expected.ProjectID, expected.Kind, expected.IdempotencyKeyHash))) != expected.ID {
					t.Fatal("idempotency removed before retirement")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProviderConditionalRetirementExcludesWritersAndPreservesReclaimedKey(t *testing.T) {
	s, expected := providerConditionalFixture(t)
	entered, resume := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.DeleteProviderResourceIfUnchanged(context.Background(), expected, func() error { close(entered); <-resume; return nil })
	}()
	<-entered
	writer := make(chan error, 1)
	go func() {
		writer <- s.update(func(tx *Tx) error {
			return tx.Bucket(bucketProviderResourceIdem).Put(providerResourceIdemKey(expected.ProjectID, expected.Kind, expected.IdempotencyKeyHash), []byte("new-owner"))
		})
	}()
	select {
	case err := <-writer:
		close(resume)
		<-done
		t.Fatal("writer crossed retirement", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProviderResource(context.Background(), expected.ProjectID, expected.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.view(func(tx *Tx) error {
		if string(tx.Bucket(bucketProviderResourceIdem).Get(providerResourceIdemKey(expected.ProjectID, expected.Kind, expected.IdempotencyKeyHash))) != "new-owner" {
			t.Fatal("new key holder lost")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
