package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

var vaultGroupRotationProgressKey = []byte("vault_group_rotation_progress")

// VaultJournalRewriteProgress is native Metadata work, not a role or common
// root grant. One fixed cursor replaces per-row markers. It changes in the
// SAME actual format2 transaction as its row, preventing double invalidation
// after a durable-write crash or a lost confirmation response.
type VaultJournalRewriteProgress struct {
	Version                        uint8  `json:"version"`
	OperationID                    string `json:"operation_id"`
	OldFingerprint, NewFingerprint string
	KeyVersion                     uint64
	Stage                          uint8
	After                          []byte
	StartedAt                      time.Time
}

var vaultGroupRewriteBuckets = [5][]byte{bucketCredentials, bucketAdminMFAAuthenticators, bucketAdminUsers, bucketAdminSessions, bucketAdminMFAChallenges}

// VerifyJournaledVaultMaterial verifies the actual final facade rows and
// target keyring in finite authenticated transactions. The supplied callbacks
// decrypt/verify only; returned values are never written by this method.
func (s *Store) VerifyJournaledVaultMaterial(options VaultRewrite) error {
	if s == nil || s.facade == nil || options.Context == nil || options.Transform == nil || options.TransformAdminMFA == nil {
		return ErrFacadeMaintenance
	}
	for stage, bucket := range vaultGroupRewriteBuckets[:2] {
		var after []byte
		for {
			done := false
			err := s.viewContext(options.Context, func(tx *Tx) error {
				b := tx.Bucket(bucket)
				if b == nil {
					return ErrVaultRewriteIncomplete
				}
				key, raw := nextVaultGroupRow(b, after)
				if key == nil {
					done = true
					return nil
				}
				if len(key) > 32768 || raw == nil || len(raw) > pricingRecoveryWideRowBytes-65536 {
					return ErrPricingRecoveryLimit
				}
				_, _, err := transformVaultGroupRow(uint8(stage), key, raw, options, time.Now().UTC())
				if err != nil {
					return err
				}
				after = bytes.Clone(key)
				return nil
			})
			if err != nil {
				return err
			}
			if done {
				break
			}
		}
	}
	return s.viewContext(options.Context, func(tx *Tx) error {
		var ring VaultKeyring
		if err := json.Unmarshal(tx.Bucket(bucketMeta).Get(keyVaultKeyring), &ring); err != nil {
			return err
		}
		if ring.Validate() != nil || ring.ActiveFingerprint != options.Keyring.ActiveFingerprint || ring.ActiveKeyVersion != options.Keyring.ActiveKeyVersion || ring.RotationOperationID != options.Keyring.RotationOperationID || ring.PreviousFingerprint != "" || len(ring.RecoveryEnvelope) != 0 {
			return ErrVaultRewriteIncomplete
		}
		return nil
	})
}

func (p VaultJournalRewriteProgress) validate(options VaultRewrite) error {
	if p.Version != 1 || !validOperationID(p.OperationID) || p.OperationID == "" || p.OperationID != options.Keyring.RotationOperationID || p.NewFingerprint != options.Keyring.ActiveFingerprint || p.OldFingerprint != options.Keyring.PreviousFingerprint || p.KeyVersion != options.Keyring.ActiveKeyVersion || p.Stage > 6 || len(p.After) > 32768 || p.StartedAt.IsZero() || p.StartedAt.Location() != time.UTC || p.Stage >= 5 && len(p.After) != 0 {
		return ErrVaultRewriteIncomplete
	}
	return nil
}

// JournaledVaultRewriteProgress reads through the original facade permission,
// actual native prefix and MAC-checked COW transaction. No legacy file mutation
// or caller-provided progress is accepted.
func (s *Store) JournaledVaultRewriteProgress(ctx context.Context) (p VaultJournalRewriteProgress, found bool, err error) {
	if s == nil || s.facade == nil {
		return p, false, ErrFacadeMaintenance
	}
	err = s.viewContext(ctx, func(tx *Tx) error {
		raw := tx.Bucket(bucketMeta).Get(vaultGroupRotationProgressKey)
		if raw == nil {
			return nil
		}
		if len(raw) > 64<<10 {
			return ErrPricingRecoveryLimit
		}
		found = true
		return json.Unmarshal(raw, &p)
	})
	if err != nil {
		return VaultJournalRewriteProgress{}, false, err
	}
	return
}

// PreflightJournaledVaultRewrite checks every old-key secret and every user
// before the caller publishes an offline intent or changes any native fact.
// Pages contain one bounded row; transformation output is validated and then
// discarded. The caller's actual stopped-group exclusion must span this scan
// and intent creation. This API supplies no such exclusion or readiness.
func (s *Store) PreflightJournaledVaultRewrite(options VaultRewrite) error {
	if s == nil || s.facade == nil || options.Context == nil {
		return ErrFacadeMaintenance
	}
	if err := validateVaultRewrite(options); err != nil {
		return err
	}
	for stage, bucket := range vaultGroupRewriteBuckets {
		var after []byte
		for {
			done := false
			err := s.viewContext(options.Context, func(tx *Tx) error {
				b := tx.Bucket(bucket)
				if b == nil {
					return ErrVaultRewriteIncomplete
				}
				key, raw := nextVaultGroupRow(b, after)
				if key == nil {
					done = true
					return nil
				}
				if len(key) > 32768 || len(raw) > pricingRecoveryWideRowBytes-65536 || raw == nil {
					return ErrPricingRecoveryLimit
				}
				_, _, err := transformVaultGroupRow(uint8(stage), key, raw, options, time.Now().UTC())
				if err != nil {
					return err
				}
				after = bytes.Clone(key)
				return options.Context.Err()
			})
			if err != nil || done {
				if err != nil {
					return err
				}
				break
			}
		}
	}
	return s.viewContext(options.Context, func(tx *Tx) error { return validateVaultGroupSourceRing(tx, options) })
}

func nextVaultGroupRow(b *Bucket, after []byte) ([]byte, []byte) {
	c := b.Cursor()
	if len(after) == 0 {
		return c.First()
	}
	k, v := c.Seek(after)
	if bytes.Equal(k, after) {
		return c.Next()
	}
	return k, v
}

func validateVaultGroupSourceRing(tx *Tx, options VaultRewrite) error {
	var ring VaultKeyring
	if err := json.Unmarshal(tx.Bucket(bucketMeta).Get(keyVaultKeyring), &ring); err != nil {
		return err
	}
	if ring.Validate() != nil || ring.ActiveFingerprint != options.Keyring.PreviousFingerprint || ring.ActiveKeyVersion == math.MaxUint64 || ring.ActiveKeyVersion+1 != options.Keyring.ActiveKeyVersion || len(ring.RecoveryEnvelope) != 0 {
		return ErrVaultRewriteIncomplete
	}
	return validateVaultRewriteDescriptor(tx, options)
}

// RewriteVaultMaterialJournaledStep performs at most ONE actual row mutation
// plus its fixed progress, or one complete key-envelope change. The original
// facade/native hooks own preparation, fsync, Ordering and confirmation; this
// helper cannot substitute a whole-file image or relax Replica writes.
func (s *Store) RewriteVaultMaterialJournaledStep(options VaultRewrite) (progress VaultJournalRewriteProgress, resultErr error) {
	if s == nil || s.facade == nil || options.Context == nil {
		return progress, ErrFacadeMaintenance
	}
	if err := validateVaultRewrite(options); err != nil {
		return progress, err
	}
	resultErr = s.updateContext(options.Context, func(tx *Tx) error {
		meta := tx.Bucket(bucketMeta)
		raw := meta.Get(vaultGroupRotationProgressKey)
		if raw == nil {
			if err := validateVaultGroupSourceRing(tx, options); err != nil {
				return err
			}
			progress = VaultJournalRewriteProgress{Version: 1, OperationID: options.Keyring.RotationOperationID, OldFingerprint: options.Keyring.PreviousFingerprint, NewFingerprint: options.Keyring.ActiveFingerprint, KeyVersion: options.Keyring.ActiveKeyVersion, StartedAt: time.Now().UTC()}
		} else {
			if len(raw) > 64<<10 {
				return ErrPricingRecoveryLimit
			}
			if err := json.Unmarshal(raw, &progress); err != nil {
				return err
			}
		}
		if progress.Stage == 6 && progress.OperationID != options.Keyring.RotationOperationID {
			prior := options
			prior.Keyring.ActiveFingerprint = progress.NewFingerprint
			prior.Keyring.PreviousFingerprint = progress.OldFingerprint
			prior.Keyring.ActiveKeyVersion = progress.KeyVersion
			prior.Keyring.RotationOperationID = progress.OperationID
			if err := progress.validate(prior); err != nil {
				return err
			}
			var current VaultKeyring
			if err := json.Unmarshal(meta.Get(keyVaultKeyring), &current); err != nil {
				return err
			}
			if current.Validate() != nil || current.ActiveFingerprint != progress.NewFingerprint || current.ActiveKeyVersion != progress.KeyVersion || current.RotationOperationID != progress.OperationID || current.PreviousFingerprint != "" || len(current.RecoveryEnvelope) != 0 {
				return ErrVaultRewriteIncomplete
			}
			if err := validateVaultGroupSourceRing(tx, options); err != nil {
				return err
			}
			progress = VaultJournalRewriteProgress{Version: 1, OperationID: options.Keyring.RotationOperationID, OldFingerprint: options.Keyring.PreviousFingerprint, NewFingerprint: options.Keyring.ActiveFingerprint, KeyVersion: options.Keyring.ActiveKeyVersion, StartedAt: time.Now().UTC()}
		}
		if err := progress.validate(options); err != nil {
			return err
		}
		if progress.Stage == 6 {
			var current VaultKeyring
			if err := json.Unmarshal(meta.Get(keyVaultKeyring), &current); err != nil {
				return err
			}
			if current.Validate() != nil || current.ActiveFingerprint != options.Keyring.ActiveFingerprint || current.ActiveKeyVersion != options.Keyring.ActiveKeyVersion || current.RotationOperationID != options.Keyring.RotationOperationID || len(current.RecoveryEnvelope) != 0 || current.PreviousFingerprint != "" {
				return ErrVaultRewriteIncomplete
			}
			return nil
		}
		if err := validateVaultGroupSourceRing(tx, options); err != nil {
			return err
		}
		if progress.Stage < 5 {
			b := tx.Bucket(vaultGroupRewriteBuckets[progress.Stage])
			if b == nil {
				return ErrVaultRewriteIncomplete
			}
			key, value := nextVaultGroupRow(b, progress.After)
			if key == nil {
				progress.Stage++
				progress.After = nil
			} else {
				if len(key) > 32768 || value == nil || len(value) > pricingRecoveryWideRowBytes-65536 {
					return ErrPricingRecoveryLimit
				}
				replacement, remove, err := transformVaultGroupRow(progress.Stage, key, value, options, progress.StartedAt)
				if err != nil {
					return err
				}
				if remove {
					err = b.Delete(key)
				} else {
					err = b.Put(key, replacement)
				}
				if err != nil {
					return err
				}
				progress.After = bytes.Clone(key)
			}
		} else {
			// File publication and independent bootstrap carry recovery keys.
			// No bridge capable of silently reviving an old DEK is published as
			// the final active keyring. The pending offline intent gates startup.
			final := options
			final.Keyring.RecoveryEnvelope = nil
			final.Keyring.PreviousFingerprint = ""
			if err := rewriteVaultMaterialMetaTx(tx, final); err != nil {
				return err
			}
			progress.Stage = 6
		}
		encoded, err := json.Marshal(progress)
		if err != nil {
			return err
		}
		if len(encoded) > 64<<10 {
			return ErrPricingRecoveryLimit
		}
		return meta.Put(vaultGroupRotationProgressKey, encoded)
	})
	if resultErr != nil {
		progress = VaultJournalRewriteProgress{}
	}
	return
}

func transformVaultGroupRow(stage uint8, key, raw []byte, options VaultRewrite, stamp time.Time) ([]byte, bool, error) {
	var value any
	switch stage {
	case 0:
		var before domain.Credential
		if err := json.Unmarshal(raw, &before); err != nil {
			return nil, false, err
		}
		if before.ID != string(key) || before.Validate() != nil {
			return nil, false, ErrVaultRewriteIncomplete
		}
		after, err := options.Transform(before)
		if err != nil {
			return nil, false, err
		}
		if after.ID != before.ID || after.Revision != before.Revision || after.Validate() != nil {
			return nil, false, ErrVaultRewriteIncomplete
		}
		value = after
	case 1:
		var before domain.AdminMFAAuthenticator
		if err := json.Unmarshal(raw, &before); err != nil {
			return nil, false, err
		}
		if adminMFAKey(before.Username, before.ID) != string(key) || before.Validate() != nil {
			return nil, false, ErrVaultRewriteIncomplete
		}
		after, err := options.TransformAdminMFA(before)
		if err != nil {
			return nil, false, err
		}
		if after.ID != before.ID || after.Username != before.Username || after.Revision != before.Revision || after.Validate() != nil {
			return nil, false, ErrVaultRewriteIncomplete
		}
		value = after
	case 2:
		var user domain.AdminUser
		if err := json.Unmarshal(raw, &user); err != nil {
			return nil, false, err
		}
		if user.Username != string(key) || user.Validate() != nil || user.Revision == math.MaxUint64 || user.SessionGeneration == math.MaxUint64 {
			return nil, false, ErrVaultRewriteIncomplete
		}
		user.SessionGeneration++
		user.Revision++
		user.UpdatedAt = stamp
		value = user
	case 3, 4:
		return nil, true, nil
	default:
		return nil, false, errors.New("invalid vault journaled rewrite stage")
	}
	encoded, err := encodeVaultRewriteRow(value, pricingRecoveryWideRowBytes-65536)
	return encoded, false, err
}
