package bolt

import (
	"bytes"
	"context"
	"encoding"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

var ErrVaultRewriteIncomplete = errors.New("unpublished vault rewrite is incomplete; discard this staging file")
var vaultRewritePendingKey = []byte("vault_rewrite_staging_pending")

// RewriteVaultMaterialStaged changes only a named unpublished whole-file
// publisher. Finite transactions may be durable before a later failure. The
// original live database remains untouched; the pending gate makes an
// accidentally published intermediate file unusable, including after a crash.
// A failed staged job is never resumed or reported successful.
func (s *Store) RewriteVaultMaterialStaged(options VaultRewrite) error {
	ctx := options.Context
	if ctx == nil || !s.publishing {
		return errors.New("staged vault rewrite requires context and an unpublished whole-file owner")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.definitionWriteMu.TryLock() {
		return errors.New("unpublished Metadata maintenance owner is busy")
	}
	defer s.definitionWriteMu.Unlock()
	leave, err := s.enter()
	if err != nil {
		return err
	}
	defer leave()
	if err := validateVaultRewrite(options); err != nil {
		return err
	}
	expectedID := 0
	// This marker belongs to the unpublished image, not the serving journal.
	// Use the existing whole-file migration entry, keeping ordinary row writes
	// subject to journal classification below.
	if err := s.migrate(func(tx *Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if tx.Bucket(bucketMeta).Get(vaultRewritePendingKey) != nil {
			return ErrVaultRewriteIncomplete
		}
		if err := validateVaultRewriteDescriptor(tx, options); err != nil {
			return err
		}
		raw := tx.Bucket(bucketMeta).Get(keySchemaVersion)
		if len(raw) != 8 || binary.BigEndian.Uint64(raw) != schemaVersion {
			return ErrSchemaVersionMismatch
		}
		if err := tx.Bucket(bucketMeta).Put(keySchemaVersion, encodeUint64(schemaVersion+1)); err != nil {
			return err
		}
		expectedID = tx.ID()
		return tx.Bucket(bucketMeta).Put(vaultRewritePendingKey, []byte{1})
	}); err != nil {
		return err
	}
	for _, bucket := range [][]byte{bucketCredentials, bucketAdminMFAAuthenticators, bucketAdminUsers, bucketAdminSessions, bucketAdminMFAChallenges} {
		var upper []byte
		if err := s.view(func(tx *Tx) error {
			if tx.ID() != expectedID {
				return ErrPricingRecoveryChanged
			}
			b := tx.Bucket(bucket)
			if b == nil {
				return ErrVaultRewriteIncomplete
			}
			key, _ := b.Cursor().Last()
			if len(key) > pricingRecoveryWideRowBytes {
				return ErrPricingRecoveryLimit
			}
			upper = bytes.Clone(key)
			return ctx.Err()
		}); err != nil {
			return err
		}
		if upper == nil {
			continue
		}
		var after []byte
		for {
			page, err := s.readPricingRecoveryPageChecked(ctx, bucket, after, upper, func(tx *Tx) error {
				if tx.ID() != expectedID {
					return ErrPricingRecoveryChanged
				}
				return nil
			})
			if err != nil {
				return err
			}
			outputBytes := 0
			for i := range page.rows {
				if err := ctx.Err(); err != nil {
					return err
				}
				row := &page.rows[i]
				switch {
				case bytes.Equal(bucket, bucketCredentials):
					var previous domain.Credential
					if err := json.Unmarshal(row.raw, &previous); err != nil {
						return err
					}
					if previous.ID != string(row.key) {
						return ErrVaultRewriteIncomplete
					}
					updated, err := options.Transform(previous)
					if err != nil {
						return err
					}
					if updated.ID != previous.ID || updated.Revision != previous.Revision {
						return ErrVaultRewriteIncomplete
					}
					if err := updated.Validate(); err != nil {
						return err
					}
					row.replacement, err = encodeVaultRewriteRow(updated, pricingRecoveryWideRowBytes-outputBytes-len(row.key))
					if err != nil {
						return err
					}
				case bytes.Equal(bucket, bucketAdminMFAAuthenticators):
					var previous domain.AdminMFAAuthenticator
					if err := json.Unmarshal(row.raw, &previous); err != nil {
						return err
					}
					if adminMFAKey(previous.Username, previous.ID) != string(row.key) {
						return ErrVaultRewriteIncomplete
					}
					updated, err := options.TransformAdminMFA(previous)
					if err != nil {
						return err
					}
					if updated.ID != previous.ID || updated.Username != previous.Username || updated.Revision != previous.Revision {
						return ErrVaultRewriteIncomplete
					}
					if err := updated.Validate(); err != nil {
						return err
					}
					row.replacement, err = encodeVaultRewriteRow(updated, pricingRecoveryWideRowBytes-outputBytes-len(row.key))
					if err != nil {
						return err
					}
				case bytes.Equal(bucket, bucketAdminUsers):
					var user domain.AdminUser
					if err := json.Unmarshal(row.raw, &user); err != nil {
						return err
					}
					if user.Username != string(row.key) || user.Revision == math.MaxUint64 || user.SessionGeneration == math.MaxUint64 {
						return ErrVaultRewriteIncomplete
					}
					user.SessionGeneration++
					user.Revision++
					user.UpdatedAt = time.Now().UTC()
					row.replacement, err = encodeVaultRewriteRow(user, pricingRecoveryWideRowBytes-outputBytes-len(row.key))
					if err != nil {
						return err
					}
				default:
					row.remove = true
				}
				outputBytes += len(row.key) + len(row.replacement)
			}
			if len(page.rows) != 0 {
				if err := s.updateContext(ctx, func(tx *Tx) error {
					if tx.ID() != expectedID+1 {
						return ErrPricingRecoveryChanged
					}
					b := tx.Bucket(bucket)
					for _, row := range page.rows {
						if err := ctx.Err(); err != nil {
							return err
						}
						if !bytes.Equal(row.raw, b.Get(row.key)) {
							return ErrPricingRecoveryChanged
						}
						if row.remove {
							if err := b.Delete(row.key); err != nil {
								return err
							}
						} else {
							if err := b.Put(row.key, row.replacement); err != nil {
								return err
							}
						}
					}
					return ctx.Err()
				}); err != nil {
					return err
				}
				expectedID++
			}
			if page.done {
				break
			}
			after = page.after
		}
	}
	return s.migrate(func(tx *Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if tx.ID() != expectedID+1 || tx.Bucket(bucketMeta).Get(vaultRewritePendingKey) == nil {
			return ErrPricingRecoveryChanged
		}
		if err := validateVaultRewriteDescriptor(tx, options); err != nil {
			return err
		}
		if err := rewriteVaultMaterialMetaTx(tx, options); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := tx.Bucket(bucketMeta).Put(keySchemaVersion, encodeUint64(schemaVersion)); err != nil {
			return err
		}
		return tx.Bucket(bucketMeta).Delete(vaultRewritePendingKey)
	})
}

// Bound the standard typed JSON graph before Encoder's escaping/base64
// allocation. No custom marshaler is invoked to discover its size. These rows
// contain only concrete domain fields and time.Time; nodes/depth are finite.
func encodeVaultRewriteRow(value any, ceiling int) ([]byte, error) {
	remaining, nodes := ceiling, 4096
	var walk func(reflect.Value, int) error
	spend := func(n int) error {
		if n < 0 || n > remaining {
			return ErrPricingRecoveryLimit
		}
		remaining -= n
		return nil
	}
	walk = func(v reflect.Value, depth int) error {
		nodes--
		if nodes < 0 || depth > 32 {
			return ErrPricingRecoveryLimit
		}
		if !v.IsValid() {
			return spend(4)
		}
		if v.Type() == reflect.TypeOf(time.Time{}) {
			return spend(40)
		}
		if v.Type() != reflect.TypeOf((*time.Time)(nil)) &&
			(v.Type().Implements(reflect.TypeOf((*json.Marshaler)(nil)).Elem()) ||
				v.Type().Implements(reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem())) {
			return ErrPricingRecoveryLimit
		}
		switch v.Kind() {
		case reflect.Pointer, reflect.Interface:
			if v.IsNil() {
				return spend(4)
			}
			return walk(v.Elem(), depth+1)
		case reflect.String:
			if v.Len() > (remaining-2)/6 {
				return ErrPricingRecoveryLimit
			}
			return spend(2 + 6*v.Len())
		case reflect.Bool:
			return spend(5)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return spend(32)
		case reflect.Slice:
			if v.Type().Elem().Kind() == reflect.Uint8 {
				if v.Len() > pricingRecoveryWideRowBytes {
					return ErrPricingRecoveryLimit
				}
				return spend(2 + 4*((v.Len()+2)/3))
			}
			if v.Len() > nodes {
				return ErrPricingRecoveryLimit
			}
			if err := spend(2 + v.Len()); err != nil {
				return err
			}
			for i := 0; i < v.Len(); i++ {
				if err := walk(v.Index(i), depth+1); err != nil {
					return err
				}
			}
			return nil
		case reflect.Struct:
			if err := spend(2); err != nil {
				return err
			}
			for i := 0; i < v.NumField(); i++ {
				field := v.Type().Field(i)
				if field.PkgPath != "" || field.Tag.Get("json") == "-" {
					continue
				}
				if err := spend(6*len(field.Name) + 4); err != nil {
					return err
				}
				if err := walk(v.Field(i), depth+1); err != nil {
					return err
				}
			}
			return nil
		case reflect.Map:
			if v.Len() > nodes {
				return ErrPricingRecoveryLimit
			}
			if err := spend(2 + v.Len()*2); err != nil {
				return err
			}
			iter := v.MapRange()
			for iter.Next() {
				if err := walk(iter.Key(), depth+1); err != nil {
					return err
				}
				if err := walk(iter.Value(), depth+1); err != nil {
					return err
				}
			}
			return nil
		default:
			return ErrPricingRecoveryLimit
		}
	}
	if err := walk(reflect.ValueOf(value), 0); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > ceiling {
		return nil, errors.Join(ErrPricingRecoveryLimit, err)
	}
	return encoded, nil
}

// Complete finite reads for offline consumers. The fixed initial TxID rejects
// even a write behind the cursor; callbacks run outside the actual read tx.
func (s *Store) walkVaultMaintenance(ctx context.Context, bucket []byte, consume func([]byte, []byte) error) error {
	var upper []byte
	id := 0
	if err := s.view(func(tx *Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		b := tx.Bucket(bucket)
		if b == nil {
			return ErrVaultRewriteIncomplete
		}
		key, _ := b.Cursor().Last()
		if len(key) > pricingRecoveryWideRowBytes {
			return ErrPricingRecoveryLimit
		}
		upper = bytes.Clone(key)
		id = tx.ID()
		return nil
	}); err != nil {
		return err
	}
	var after []byte
	if upper != nil {
		for {
			page, err := s.readPricingRecoveryPageChecked(ctx, bucket, after, upper, func(tx *Tx) error {
				if tx.ID() != id {
					return ErrPricingRecoveryChanged
				}
				return nil
			})
			if err != nil {
				return err
			}
			for _, row := range page.rows {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := consume(row.key, row.raw); err != nil {
					return err
				}
			}
			if page.done {
				break
			}
			after = page.after
		}
	}
	return s.view(func(tx *Tx) error {
		if tx.ID() != id {
			return ErrPricingRecoveryChanged
		}
		return ctx.Err()
	})
}

func (s *Store) WalkCredentialsMaintenance(ctx context.Context, consume func(domain.Credential) error) error {
	if consume == nil {
		return errors.New("credential maintenance consumer is required")
	}
	return s.walkVaultMaintenance(ctx, bucketCredentials, func(key, raw []byte) error {
		var value domain.Credential
		if err := decodeMetadataMaintenanceJSON(ctx, raw, &value); err != nil {
			return err
		}
		if value.ID == "" || value.ID != string(key) {
			return ErrVaultRewriteIncomplete
		}
		return consume(value)
	})
}
func (s *Store) WalkAdminMFAAuthenticatorsMaintenance(ctx context.Context, consume func(domain.AdminMFAAuthenticator) error) error {
	if consume == nil {
		return errors.New("MFA maintenance consumer is required")
	}
	return s.walkVaultMaintenance(ctx, bucketAdminMFAAuthenticators, func(key, raw []byte) error {
		var value domain.AdminMFAAuthenticator
		if err := decodeMetadataMaintenanceJSON(ctx, raw, &value); err != nil {
			return err
		}
		if value.ID == "" || adminMFAKey(value.Username, value.ID) != string(key) {
			return ErrVaultRewriteIncomplete
		}
		return consume(value)
	})
}
