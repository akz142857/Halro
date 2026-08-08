package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/masterkey"
	bbolt "go.etcd.io/bbolt"
)

func (s *Store) PutVaultKeyCheck(value []byte) error {
	if len(value) == 0 {
		return errors.New("vault key check cannot be empty")
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if meta.Get(keyVaultCheck) != nil {
			return ErrAlreadyExists
		}
		return meta.Put(keyVaultCheck, value)
	})
}

func (s *Store) VaultKeyCheck() ([]byte, error) {
	var value []byte
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyVaultCheck)
		if raw == nil {
			return ErrNotFound
		}
		value = bytes.Clone(raw)
		return nil
	})
	return value, err
}

func (s *Store) PutVaultKeyring(keyring VaultKeyring) error {
	if err := keyring.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(keyring)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if meta.Get(keyVaultKeyring) != nil {
			return ErrAlreadyExists
		}
		return meta.Put(keyVaultKeyring, encoded)
	})
}

func (s *Store) VaultKeyring() (VaultKeyring, error) {
	var keyring VaultKeyring
	raw, err := s.metaBytes(keyVaultKeyring)
	if err != nil {
		return keyring, err
	}
	if err := json.Unmarshal(raw, &keyring); err != nil {
		return keyring, fmt.Errorf("decode vault keyring: %w", err)
	}
	return keyring, keyring.Validate()
}

func (s *Store) PutKeySlotDescriptor(ctx context.Context, descriptor masterkey.KeySlotDescriptor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := descriptor.Validate(); err != nil {
		return err
	}
	// Only a pristine descriptor may be created directly. Every subsequent
	// mutation must pass through the operation-specific methods below so an
	// active slot cannot be fabricated without unwrap and candidate verification.
	if descriptor.Revision != 1 || descriptor.ActiveGeneration != 1 || len(descriptor.Slots) != 0 {
		return errors.New("initial key slot descriptor must be pristine")
	}
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		if meta.Get(keyKeySlotDescriptor) != nil {
			return ErrAlreadyExists
		}
		return meta.Put(keyKeySlotDescriptor, encoded)
	})
}

func (s *Store) KeySlotDescriptor(ctx context.Context) (masterkey.KeySlotDescriptor, error) {
	var descriptor masterkey.KeySlotDescriptor
	if err := ctx.Err(); err != nil {
		return descriptor, err
	}
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyKeySlotDescriptor)
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &descriptor); err != nil {
			return fmt.Errorf("decode key slot descriptor: %w", err)
		}
		return descriptor.Validate()
	})
	return descriptor.Clone(), err
}

func (s *Store) AddKeySlot(
	ctx context.Context,
	pending masterkey.PendingKeySlot,
	expectedRevision uint64,
	now time.Time,
) (masterkey.KeySlotDescriptor, *masterkey.SlotTransition, error) {
	current, err := s.KeySlotDescriptor(ctx)
	if err != nil {
		return masterkey.KeySlotDescriptor{}, nil, err
	}
	next, transition, err := current.AddSlot(pending, expectedRevision, now)
	if err != nil || transition == nil {
		return next, transition, err
	}
	if err := s.replaceKeySlotDescriptor(ctx, current.Revision, next); err != nil {
		return masterkey.KeySlotDescriptor{}, nil, err
	}
	return next, transition, nil
}

func (s *Store) VerifyKeySlot(
	ctx context.Context,
	slotID string,
	expectedDescriptorRevision uint64,
	expectedSlotRevision uint64,
	unwrapper masterkey.SlotUnwrapper,
	verifier masterkey.CandidateVerifier,
	now time.Time,
) (masterkey.KeySlotDescriptor, *masterkey.SlotTransition, error) {
	current, err := s.KeySlotDescriptor(ctx)
	if err != nil {
		return masterkey.KeySlotDescriptor{}, nil, err
	}
	next, transition, err := current.VerifySlot(
		ctx, slotID, expectedDescriptorRevision, expectedSlotRevision, unwrapper, verifier, now,
	)
	if err != nil || transition == nil {
		return next, transition, err
	}
	if err := s.replaceKeySlotDescriptor(ctx, current.Revision, next); err != nil {
		return masterkey.KeySlotDescriptor{}, nil, err
	}
	return next, transition, nil
}

func (s *Store) RetireKeySlot(
	ctx context.Context,
	slotID string,
	expectedDescriptorRevision uint64,
	expectedSlotRevision uint64,
	now time.Time,
) (masterkey.KeySlotDescriptor, *masterkey.SlotTransition, error) {
	return s.transitionKeySlot(ctx, slotID, expectedDescriptorRevision, expectedSlotRevision, masterkey.KeySlotRetiring, now)
}

func (s *Store) RevokeKeySlot(
	ctx context.Context,
	slotID string,
	expectedDescriptorRevision uint64,
	expectedSlotRevision uint64,
	now time.Time,
) (masterkey.KeySlotDescriptor, *masterkey.SlotTransition, error) {
	return s.transitionKeySlot(ctx, slotID, expectedDescriptorRevision, expectedSlotRevision, masterkey.KeySlotRevoked, now)
}

func (s *Store) transitionKeySlot(
	ctx context.Context,
	slotID string,
	expectedDescriptorRevision uint64,
	expectedSlotRevision uint64,
	target masterkey.KeySlotState,
	now time.Time,
) (masterkey.KeySlotDescriptor, *masterkey.SlotTransition, error) {
	current, err := s.KeySlotDescriptor(ctx)
	if err != nil {
		return masterkey.KeySlotDescriptor{}, nil, err
	}
	var next masterkey.KeySlotDescriptor
	var transition *masterkey.SlotTransition
	switch target {
	case masterkey.KeySlotRetiring:
		next, transition, err = current.RetireSlot(slotID, expectedDescriptorRevision, expectedSlotRevision, now)
	case masterkey.KeySlotRevoked:
		next, transition, err = current.RevokeSlot(slotID, expectedDescriptorRevision, expectedSlotRevision, now)
	default:
		return masterkey.KeySlotDescriptor{}, nil, masterkey.ErrInvalidTransition
	}
	if err != nil || transition == nil {
		return next, transition, err
	}
	if err := s.replaceKeySlotDescriptor(ctx, current.Revision, next); err != nil {
		return masterkey.KeySlotDescriptor{}, nil, err
	}
	return next, transition, nil
}

func (s *Store) replaceKeySlotDescriptor(
	ctx context.Context,
	expectedRevision uint64,
	descriptor masterkey.KeySlotDescriptor,
) error {
	return s.replaceKeySlotDescriptorWithHook(ctx, expectedRevision, descriptor, nil)
}

func (s *Store) InitializeKeySlotState(ctx context.Context, state KeySlotInitialization) error {
	return s.initializeKeySlotStateWithHook(ctx, state, nil)
}

func (s *Store) initializeKeySlotStateWithHook(ctx context.Context, state KeySlotInitialization, hook func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := state.Descriptor.Validate(); err != nil {
		return err
	}
	if !state.Descriptor.ProductionReady() {
		return errors.New("initial key slot descriptor must be production-ready")
	}
	if err := state.Keyring.Validate(); err != nil {
		return err
	}
	if state.Keyring.ActiveFingerprint != state.Descriptor.MasterKeyFingerprint {
		return errors.New("keyring fingerprint does not match key slot descriptor")
	}
	if state.Unwrapper == nil || state.Verifier == nil {
		return errors.New("key slot initialization requires unwrap and candidate verification")
	}
	for _, slot := range state.Descriptor.Slots {
		if slot.State != masterkey.KeySlotActive || slot.VerifiedAt == nil {
			return errors.New("initial key slot descriptor contains an unverified slot")
		}
		candidate, err := state.Unwrapper.Unwrap(ctx, slot)
		if err != nil {
			return fmt.Errorf("verify initial key slot %q: %w", slot.ID, err)
		}
		fingerprint, fingerprintErr := masterkey.MasterKeyFingerprint(candidate)
		verifyErr := state.Verifier.VerifyCandidate(ctx, candidate)
		clear(candidate)
		if fingerprintErr != nil {
			return fmt.Errorf("verify initial key slot %q fingerprint: %w", slot.ID, fingerprintErr)
		}
		if fingerprint != state.Descriptor.MasterKeyFingerprint {
			return fmt.Errorf("verify initial key slot %q: %w", slot.ID, masterkey.ErrVaultKeyMismatch)
		}
		if verifyErr != nil {
			return fmt.Errorf("verify initial key slot %q candidate: %w", slot.ID, verifyErr)
		}
	}
	if len(state.VaultKeyCheck) == 0 || len(state.AuditHMACEnvelope) == 0 || state.AuditCheckpoint.Bytes < 0 ||
		len(state.LedgerHMACEnvelope) == 0 {
		return errors.New("complete Vault, Audit and Ledger initialization material is required")
	}
	descriptor, err := json.Marshal(state.Descriptor)
	if err != nil {
		return err
	}
	keyring, err := json.Marshal(state.Keyring)
	if err != nil {
		return err
	}
	checkpoint, err := json.Marshal(state.AuditCheckpoint)
	if err != nil {
		return err
	}
	values := []struct {
		name  string
		key   []byte
		value []byte
	}{
		{name: "descriptor", key: keyKeySlotDescriptor, value: descriptor},
		{name: "keyring", key: keyVaultKeyring, value: keyring},
		{name: "vault_key_check", key: keyVaultCheck, value: state.VaultKeyCheck},
		{name: "audit_hmac_envelope", key: keyAuditHMACEnvelope, value: state.AuditHMACEnvelope},
		{name: "audit_checkpoint", key: keyAuditCheckpoint, value: checkpoint},
		{name: "ledger_hmac_envelope", key: keyLedgerHMACEnvelope, value: state.LedgerHMACEnvelope},
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		for _, item := range values {
			if meta.Get(item.key) != nil {
				return ErrAlreadyExists
			}
		}
		for _, item := range values {
			if hook != nil {
				if err := hook("before_put_" + item.name); err != nil {
					return err
				}
			}
			if err := meta.Put(item.key, item.value); err != nil {
				return err
			}
			if hook != nil {
				if err := hook("after_put_" + item.name); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *Store) replaceKeySlotDescriptorWithHook(
	ctx context.Context,
	expectedRevision uint64,
	descriptor masterkey.KeySlotDescriptor,
	hook func(string) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if expectedRevision == 0 {
		return errors.New("expected key slot descriptor revision is required")
	}
	if err := descriptor.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		raw := meta.Get(keyKeySlotDescriptor)
		if raw == nil {
			return ErrNotFound
		}
		var current masterkey.KeySlotDescriptor
		if err := json.Unmarshal(raw, &current); err != nil {
			return fmt.Errorf("decode current key slot descriptor: %w", err)
		}
		if err := current.Validate(); err != nil {
			return err
		}
		if current.Revision != expectedRevision {
			return ErrRevisionConflict
		}
		if err := descriptor.ValidateSuccessor(current); err != nil {
			return err
		}
		if hook != nil {
			if err := hook("before_put_key_slot_descriptor"); err != nil {
				return err
			}
		}
		if err := meta.Put(keyKeySlotDescriptor, encoded); err != nil {
			return err
		}
		if hook != nil {
			if err := hook("after_put_key_slot_descriptor"); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) VaultRotationBridge() ([]byte, error) {
	keyring, err := s.VaultKeyring()
	if err != nil {
		return nil, err
	}
	if len(keyring.RecoveryEnvelope) == 0 {
		return nil, ErrNotFound
	}
	return bytes.Clone(keyring.RecoveryEnvelope), nil
}

// RewriteVaultMaterial runs only against an offline copy of metadata. All
// credential ciphertext and system envelopes change in one bbolt transaction.
func (s *Store) RewriteVaultMaterial(options VaultRewrite) error {
	if len(options.VaultKeyCheck) == 0 || len(options.AuditHMACEnvelope) == 0 ||
		len(options.LedgerHMACEnvelope) == 0 ||
		options.Transform == nil || options.TransformAdminMFA == nil || options.Keyring.Validate() != nil ||
		len(options.Keyring.RecoveryEnvelope) == 0 {
		return errors.New("complete vault rewrite material is required")
	}
	if options.KeySlotDescriptor != nil {
		if options.Context == nil || options.Unwrapper == nil || options.Verifier == nil {
			return errors.New("rotated key slot descriptor requires unwrap and candidate verification")
		}
		if err := options.KeySlotDescriptor.Validate(); err != nil {
			return err
		}
		if options.KeySlotDescriptor.MasterKeyFingerprint != options.Keyring.ActiveFingerprint {
			return errors.New("rotated descriptor and keyring fingerprints do not match")
		}
		for _, slot := range options.KeySlotDescriptor.Slots {
			candidate, err := options.Unwrapper.Unwrap(options.Context, slot)
			if err != nil {
				return fmt.Errorf("verify rotated key slot %q: %w", slot.ID, err)
			}
			fingerprint, fingerprintErr := masterkey.MasterKeyFingerprint(candidate)
			verifyErr := options.Verifier.VerifyCandidate(options.Context, candidate)
			clear(candidate)
			if fingerprintErr != nil {
				return fmt.Errorf("verify rotated key slot %q fingerprint: %w", slot.ID, fingerprintErr)
			}
			if fingerprint != options.KeySlotDescriptor.MasterKeyFingerprint {
				return fmt.Errorf("verify rotated key slot %q: %w", slot.ID, masterkey.ErrVaultKeyMismatch)
			}
			if verifyErr != nil {
				return fmt.Errorf("verify rotated key slot %q candidate: %w", slot.ID, verifyErr)
			}
		}
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		if options.KeySlotDescriptor != nil {
			raw := tx.Bucket(bucketMeta).Get(keyKeySlotDescriptor)
			if raw == nil {
				return ErrNotFound
			}
			var previous masterkey.KeySlotDescriptor
			if err := json.Unmarshal(raw, &previous); err != nil {
				return fmt.Errorf("decode previous key slot descriptor: %w", err)
			}
			if err := options.KeySlotDescriptor.ValidateRotationSuccessor(previous); err != nil {
				return err
			}
		}
		credentials := tx.Bucket(bucketCredentials)
		cursor := credentials.Cursor()
		for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
			var credential domain.Credential
			if err := json.Unmarshal(raw, &credential); err != nil {
				return fmt.Errorf("decode credential %q: %w", key, err)
			}
			updated, err := options.Transform(credential)
			if err != nil {
				return fmt.Errorf("rewrite credential %q: %w", key, err)
			}
			if updated.ID != credential.ID || updated.Revision != credential.Revision {
				return errors.New("vault rewrite cannot change credential identity or revision")
			}
			if err := updated.Validate(); err != nil {
				return err
			}
			encoded, err := json.Marshal(updated)
			if err != nil {
				return err
			}
			if err := credentials.Put(key, encoded); err != nil {
				return err
			}
		}
		authenticators := tx.Bucket(bucketAdminMFAAuthenticators)
		mfaCursor := authenticators.Cursor()
		for key, raw := mfaCursor.First(); key != nil; key, raw = mfaCursor.Next() {
			var value domain.AdminMFAAuthenticator
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			updated, err := options.TransformAdminMFA(value)
			if err != nil {
				return err
			}
			if updated.ID != value.ID || updated.Username != value.Username || updated.Revision != value.Revision {
				return errors.New("vault rewrite cannot change MFA identity or revision")
			}
			if err = updated.Validate(); err != nil {
				return err
			}
			encoded, err := json.Marshal(updated)
			if err != nil {
				return err
			}
			if err = authenticators.Put(key, encoded); err != nil {
				return err
			}
		}
		meta := tx.Bucket(bucketMeta)
		if err := meta.Put(keyVaultCheck, options.VaultKeyCheck); err != nil {
			return err
		}
		if err := meta.Put(keyAuditHMACEnvelope, options.AuditHMACEnvelope); err != nil {
			return err
		}
		if err := meta.Put(keyLedgerHMACEnvelope, options.LedgerHMACEnvelope); err != nil {
			return err
		}
		encodedKeyring, err := json.Marshal(options.Keyring)
		if err != nil {
			return err
		}
		if err := meta.Put(keyVaultKeyring, encodedKeyring); err != nil {
			return err
		}
		if options.KeySlotDescriptor != nil {
			encodedDescriptor, err := json.Marshal(options.KeySlotDescriptor)
			if err != nil {
				return err
			}
			if err := meta.Put(keyKeySlotDescriptor, encodedDescriptor); err != nil {
				return err
			}
		}
		// Master-key rotation invalidates every active Admin identity and
		// pre-auth challenge in the same transaction as the ciphertext rewrite.
		users := tx.Bucket(bucketAdminUsers)
		userCursor := users.Cursor()
		for key, raw := userCursor.First(); key != nil; key, raw = userCursor.Next() {
			var user domain.AdminUser
			if err := json.Unmarshal(raw, &user); err != nil {
				return err
			}
			user.SessionGeneration++
			user.Revision++
			user.UpdatedAt = time.Now().UTC()
			encoded, err := json.Marshal(user)
			if err != nil {
				return err
			}
			if err := users.Put(key, encoded); err != nil {
				return err
			}
		}
		sessions := tx.Bucket(bucketAdminSessions)
		sessionCursor := sessions.Cursor()
		for key, _ := sessionCursor.First(); key != nil; key, _ = sessionCursor.Next() {
			if err := sessionCursor.Delete(); err != nil {
				return err
			}
		}
		challenges := tx.Bucket(bucketAdminMFAChallenges)
		challengeCursor := challenges.Cursor()
		for key, _ := challengeCursor.First(); key != nil; key, _ = challengeCursor.Next() {
			if err := challengeCursor.Delete(); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) ClearVaultRotationBridge() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		raw := meta.Get(keyVaultKeyring)
		if raw == nil {
			return ErrNotFound
		}
		var keyring VaultKeyring
		if err := json.Unmarshal(raw, &keyring); err != nil {
			return err
		}
		if err := keyring.Validate(); err != nil {
			return err
		}
		keyring.RecoveryEnvelope = nil
		encoded, err := json.Marshal(keyring)
		if err != nil {
			return err
		}
		return meta.Put(keyVaultKeyring, encoded)
	})
}
