package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/akz142857/Halro/internal/domain"
)

// DeleteProviderResourceIfUnchanged protects a maintenance observation from a
// later revision (including changes that keep its object path). retire runs
// under the real Metadata writer and must not reenter Store. Failed retirement
// rolls back the record; a late journal failure may leave the already unreadable
// legacy object absent and the row present, never an unconfirmed success.
func (s *Store) DeleteProviderResourceIfUnchanged(ctx context.Context, expected domain.ProviderResource, retire func() error) error {
	if expected.ID == "" || expected.ProjectID == "" || expected.Revision == 0 || retire == nil {
		return ErrRevisionConflict
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.updateContext(ctx, func(tx *Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw := tx.Bucket(bucketProviderResources).Get([]byte(expected.ID))
		if raw == nil {
			return ErrNotFound
		}
		if len(raw) > pricingRecoveryWideRowBytes {
			return ErrRevisionConflict
		}
		var current domain.ProviderResource
		if err := json.Unmarshal(raw, &current); err != nil {
			return err
		}
		if !reflect.DeepEqual(current, expected) {
			return ErrRevisionConflict
		}
		if err := retire(); err != nil {
			return err
		}
		return deleteProviderResourceTx(tx, current)
	})
}

// WalkProviderResourcesMaintenance copies <=64 rows/1MiB (one legacy row
// <=8MiB) and releases each reader before cleanup can write Metadata. Its
// initial upper key makes a pass finite under incoming traffic. It is a live
// maintenance walk, not a complete frozen source or a deletion capability.
func (s *Store) WalkProviderResourcesMaintenance(ctx context.Context, consume func(domain.ProviderResource) error) error {
	if consume == nil {
		return errors.New("provider maintenance consumer is required")
	}
	var upper []byte
	if err := s.view(func(tx *Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		b := tx.Bucket(bucketProviderResources)
		if b == nil {
			return errors.New("provider resource bucket is absent")
		}
		key, _ := b.Cursor().Last()
		upper = bytes.Clone(key)
		return nil
	}); err != nil || upper == nil {
		return err
	}
	var after []byte
	for {
		page, err := s.readPricingRecoveryPageChecked(ctx, bucketProviderResources, after, upper, nil)
		if err != nil {
			return err
		}
		for _, row := range page.rows {
			if err := ctx.Err(); err != nil {
				return err
			}
			var resource domain.ProviderResource
			if err := json.Unmarshal(row.raw, &resource); err != nil {
				return err
			}
			if resource.ID == "" || resource.ID != string(row.key) {
				return errors.New("provider maintenance resource identity mismatch")
			}
			if err := consume(resource); err != nil {
				return err
			}
		}
		if page.done {
			return ctx.Err()
		}
		after = page.after
	}
}

// WithProviderObjectReferenceSnapshot excludes Metadata writers while a finite
// file retirement uses an already complete reference set of this exact TxID.
// The raw writable transaction is ALWAYS rolled back: it changes no data,
// sequence, native journal or TxID. The callback must not reenter this Store.
// A read transaction alone would not exclude new references during unlink.
func (s *Store) WithProviderObjectReferenceSnapshot(ctx context.Context, snapshotTxID uint64, retire func() error) (resultErr error) {
	if s != nil && s.facade != nil {
		return ErrFacadeMaintenance
	}
	if snapshotTxID == 0 || retire == nil {
		return ErrProviderBackupChanged
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	leave, err := s.enter()
	if err != nil {
		return err
	}
	defer leave()
	tx, err := s.db.Begin(true)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, tx.Rollback()) }()
	if err := errors.Join(s.RecoveryError(), ctx.Err()); err != nil {
		return err
	}
	if tx.ID() <= 0 || uint64(tx.ID()-1) != snapshotTxID {
		return ErrProviderBackupChanged
	}
	return retire()
}
