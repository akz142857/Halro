package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/masterkey"
	bbolt "go.etcd.io/bbolt"
)

func (s *Store) PutAuditHMACEnvelope(value []byte) error {
	if len(value) == 0 {
		return errors.New("audit HMAC envelope cannot be empty")
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if meta.Get(keyAuditHMACEnvelope) != nil {
			return ErrAlreadyExists
		}
		return meta.Put(keyAuditHMACEnvelope, value)
	})
}

func (s *Store) AuditHMACEnvelope() ([]byte, error) {
	return s.metaBytes(keyAuditHMACEnvelope)
}

func (s *Store) AddKeySlotWithAuditIntent(
	ctx context.Context,
	pending masterkey.PendingKeySlot,
	expectedRevision uint64,
	now time.Time,
) (masterkey.KeySlotDescriptor, masterkey.KeySlotAuditIntent, error) {
	current, err := s.KeySlotDescriptor(ctx)
	if err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	next, transition, err := current.AddSlot(pending, expectedRevision, now)
	if err != nil || transition == nil {
		return next, masterkey.KeySlotAuditIntent{}, err
	}
	intent, err := newKeySlotAuditIntent(transition, expectedRevision, 0, "")
	if err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	if err := s.replaceKeySlotDescriptorWithAuditIntent(ctx, current, next, intent); err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	return next, intent, nil
}

func (s *Store) VerifyKeySlotWithAuditIntent(
	ctx context.Context,
	slotID string,
	expectedDescriptorRevision uint64,
	expectedSlotRevision uint64,
	unwrapper masterkey.SlotUnwrapper,
	verifier masterkey.CandidateVerifier,
	now time.Time,
) (masterkey.KeySlotDescriptor, masterkey.KeySlotAuditIntent, error) {
	current, err := s.KeySlotDescriptor(ctx)
	if err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	next, transition, err := current.VerifySlot(ctx, slotID, expectedDescriptorRevision, expectedSlotRevision, unwrapper, verifier, now)
	if err != nil || transition == nil {
		return next, masterkey.KeySlotAuditIntent{}, err
	}
	intent, err := newKeySlotAuditIntent(transition, expectedDescriptorRevision, expectedSlotRevision, "")
	if err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	if err := s.replaceKeySlotDescriptorWithAuditIntent(ctx, current, next, intent); err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	return next, intent, nil
}

func (s *Store) RetireKeySlotWithAuditIntent(
	ctx context.Context,
	slotID string,
	expectedDescriptorRevision uint64,
	expectedSlotRevision uint64,
	now time.Time,
) (masterkey.KeySlotDescriptor, masterkey.KeySlotAuditIntent, error) {
	current, err := s.KeySlotDescriptor(ctx)
	if err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	next, transition, err := current.RetireSlot(slotID, expectedDescriptorRevision, expectedSlotRevision, now)
	if err != nil || transition == nil {
		return next, masterkey.KeySlotAuditIntent{}, err
	}
	intent, err := newKeySlotAuditIntent(transition, expectedDescriptorRevision, expectedSlotRevision, "")
	if err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	if err := s.replaceKeySlotDescriptorWithAuditIntent(ctx, current, next, intent); err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	return next, intent, nil
}

func (s *Store) RevokeKeySlotWithAuditIntent(
	ctx context.Context,
	slotID string,
	expectedDescriptorRevision uint64,
	expectedSlotRevision uint64,
	reasonCode string,
	now time.Time,
) (masterkey.KeySlotDescriptor, masterkey.KeySlotAuditIntent, error) {
	current, err := s.KeySlotDescriptor(ctx)
	if err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	next, transition, err := current.RevokeSlot(slotID, expectedDescriptorRevision, expectedSlotRevision, now)
	if err != nil || transition == nil {
		return next, masterkey.KeySlotAuditIntent{}, err
	}
	intent, err := newKeySlotAuditIntent(transition, expectedDescriptorRevision, expectedSlotRevision, reasonCode)
	if err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	if err := s.replaceKeySlotDescriptorWithAuditIntent(ctx, current, next, intent); err != nil {
		return masterkey.KeySlotDescriptor{}, masterkey.KeySlotAuditIntent{}, err
	}
	return next, intent, nil
}

func newKeySlotAuditIntent(transition *masterkey.SlotTransition, expectedDescriptorRevision, expectedSlotRevision uint64, reasonCode string) (masterkey.KeySlotAuditIntent, error) {
	if transition == nil {
		return masterkey.KeySlotAuditIntent{}, errors.New("Key Slot transition is required")
	}
	intent := masterkey.KeySlotAuditIntent{
		EventID:    fmt.Sprintf("aud-slot-%s-%d-%d", transition.SlotID, transition.DescriptorRevision, transition.SlotRevision),
		OccurredAt: transition.OccurredAt, Action: transition.AuditAction(), TargetID: transition.SlotID,
		Purpose: transition.Purpose, ReasonCode: reasonCode,
		ExpectedDescriptorRevision: expectedDescriptorRevision, ExpectedSlotRevision: expectedSlotRevision,
		DescriptorRevision: transition.DescriptorRevision, SlotRevision: transition.SlotRevision,
	}
	return intent, intent.Validate()
}

func (s *Store) replaceKeySlotDescriptorWithAuditIntent(ctx context.Context, current, next masterkey.KeySlotDescriptor, intent masterkey.KeySlotAuditIntent) error {
	if err := intent.Validate(); err != nil {
		return err
	}
	if err := next.ValidateSuccessor(current); err != nil {
		return err
	}
	descriptorPayload, err := json.Marshal(next)
	if err != nil {
		return err
	}
	intentPayload, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		if raw := meta.Get(keyKeySlotAuditIntent); raw != nil {
			var previous masterkey.KeySlotAuditIntent
			if err := json.Unmarshal(raw, &previous); err != nil {
				return err
			}
			if err := previous.Validate(); err != nil {
				return err
			}
			if !previous.Delivered {
				return errors.New("a previous Key Slot audit event is still pending delivery")
			}
		}
		raw := meta.Get(keyKeySlotDescriptor)
		if raw == nil {
			return ErrNotFound
		}
		var persisted masterkey.KeySlotDescriptor
		if err := json.Unmarshal(raw, &persisted); err != nil {
			return err
		}
		if persisted.Revision != current.Revision {
			return ErrRevisionConflict
		}
		if err := persisted.Validate(); err != nil {
			return err
		}
		if err := next.ValidateSuccessor(persisted); err != nil {
			return err
		}
		if err := meta.Put(keyKeySlotDescriptor, descriptorPayload); err != nil {
			return err
		}
		return meta.Put(keyKeySlotAuditIntent, intentPayload)
	})
}

func (s *Store) KeySlotAuditIntent() (masterkey.KeySlotAuditIntent, error) {
	var intent masterkey.KeySlotAuditIntent
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyKeySlotAuditIntent)
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &intent); err != nil {
			return err
		}
		return intent.Validate()
	})
	return intent, err
}

func (s *Store) MarkKeySlotAuditDelivered(ctx context.Context, eventID string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		raw := meta.Get(keyKeySlotAuditIntent)
		if raw == nil {
			return ErrNotFound
		}
		var intent masterkey.KeySlotAuditIntent
		if err := json.Unmarshal(raw, &intent); err != nil {
			return err
		}
		if err := intent.Validate(); err != nil {
			return err
		}
		if intent.EventID != eventID {
			return ErrRevisionConflict
		}
		intent.Delivered = true
		payload, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		return meta.Put(keyKeySlotAuditIntent, payload)
	})
}

func (s *Store) EnsureMasterKeyRotationAuditIntent(ctx context.Context, operationID string, now time.Time) (masterkey.MasterKeyRotationAuditIntent, error) {
	requested, err := masterkey.NewMasterKeyRotationAuditIntent(operationID, now)
	if err != nil {
		return masterkey.MasterKeyRotationAuditIntent{}, err
	}
	var result masterkey.MasterKeyRotationAuditIntent
	err = s.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		if raw := meta.Get(keyMasterKeyRotationAuditIntent); raw != nil {
			if err := json.Unmarshal(raw, &result); err != nil {
				return err
			}
			if err := result.Validate(); err != nil {
				return err
			}
			if result.OperationID == operationID {
				return nil
			}
			if !result.CompletedDelivered {
				return errors.New("a different Master Key rotation Audit intent is still pending")
			}
		}
		payload, err := json.Marshal(requested)
		if err != nil {
			return err
		}
		if err := meta.Put(keyMasterKeyRotationAuditIntent, payload); err != nil {
			return err
		}
		result = requested
		return nil
	})
	return result, err
}

func (s *Store) MasterKeyRotationAuditIntent() (masterkey.MasterKeyRotationAuditIntent, error) {
	var intent masterkey.MasterKeyRotationAuditIntent
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyMasterKeyRotationAuditIntent)
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &intent); err != nil {
			return err
		}
		return intent.Validate()
	})
	return intent, err
}

func (s *Store) MarkMasterKeyRotationAuditDelivered(ctx context.Context, eventID string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		raw := meta.Get(keyMasterKeyRotationAuditIntent)
		if raw == nil {
			return ErrNotFound
		}
		var intent masterkey.MasterKeyRotationAuditIntent
		if err := json.Unmarshal(raw, &intent); err != nil {
			return err
		}
		if err := intent.Validate(); err != nil {
			return err
		}
		switch eventID {
		case intent.StartedEventID:
			intent.StartedDelivered = true
		case intent.CompletedEventID:
			if intent.CompletedAt == nil || !intent.StartedDelivered {
				return ErrRevisionConflict
			}
			intent.CompletedDelivered = true
		default:
			return ErrRevisionConflict
		}
		payload, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		return meta.Put(keyMasterKeyRotationAuditIntent, payload)
	})
}

func (s *Store) ClearVaultRotationBridgeWithAuditIntent(ctx context.Context, operationID string, now time.Time) (masterkey.MasterKeyRotationAuditIntent, error) {
	var completed masterkey.MasterKeyRotationAuditIntent
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		rawKeyring := meta.Get(keyVaultKeyring)
		if rawKeyring == nil {
			return ErrNotFound
		}
		var keyring VaultKeyring
		if err := json.Unmarshal(rawKeyring, &keyring); err != nil {
			return err
		}
		if err := keyring.Validate(); err != nil {
			return err
		}
		if keyring.RotationOperationID != operationID || len(keyring.RecoveryEnvelope) == 0 {
			return errors.New("Master Key rotation bridge does not match the requested operation")
		}
		rawIntent := meta.Get(keyMasterKeyRotationAuditIntent)
		if rawIntent == nil {
			return ErrNotFound
		}
		var intent masterkey.MasterKeyRotationAuditIntent
		if err := json.Unmarshal(rawIntent, &intent); err != nil {
			return err
		}
		if intent.OperationID != operationID {
			return ErrRevisionConflict
		}
		var err error
		completed, err = intent.WithCompletion(now)
		if err != nil {
			return err
		}
		keyring.RecoveryEnvelope = nil
		encodedKeyring, err := json.Marshal(keyring)
		if err != nil {
			return err
		}
		encodedIntent, err := json.Marshal(completed)
		if err != nil {
			return err
		}
		if err := meta.Put(keyVaultKeyring, encodedKeyring); err != nil {
			return err
		}
		return meta.Put(keyMasterKeyRotationAuditIntent, encodedIntent)
	})
	return completed, err
}

func (s *Store) PutAuditCheckpoint(checkpoint AuditCheckpoint) error {
	if checkpoint.Bytes < 0 || (checkpoint.Records == 0 && (checkpoint.Bytes != 0 ||
		checkpoint.LastHash != [32]byte{})) ||
		(checkpoint.Records > 0 && (checkpoint.Bytes == 0 || checkpoint.LastHash == [32]byte{})) {
		return errors.New("audit checkpoint is invalid")
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if raw := meta.Get(keyAuditCheckpoint); raw != nil {
			var current AuditCheckpoint
			if err := json.Unmarshal(raw, &current); err != nil {
				return fmt.Errorf("decode current audit checkpoint: %w", err)
			}
			if checkpoint.Records < current.Records {
				return errors.New("audit checkpoint cannot move backwards")
			}
			if checkpoint.Records == current.Records && checkpoint != current {
				return errors.New("audit checkpoint conflicts at the same sequence")
			}
		}
		return meta.Put(keyAuditCheckpoint, encoded)
	})
}

func (s *Store) AuditCheckpoint() (AuditCheckpoint, error) {
	var checkpoint AuditCheckpoint
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyAuditCheckpoint)
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &checkpoint); err != nil {
			return fmt.Errorf("decode audit checkpoint: %w", err)
		}
		return nil
	})
	return checkpoint, err
}

func (s *Store) CreateDeploymentPriceVersionWithAuditIntent(ctx context.Context, price domain.DeploymentPriceVersion, intent domain.PricingAuditIntent) (domain.DeploymentPriceVersion, error) {
	if err := intent.Validate(); err != nil {
		return domain.DeploymentPriceVersion{}, err
	}
	if intent.Action != "deployment_price.create" || intent.TargetID != price.ID {
		return domain.DeploymentPriceVersion{}, errors.New("pricing audit intent does not match price creation")
	}
	created, _, _, err := s.createDeploymentPriceVersion(ctx, price, &intent, "")
	return created, err
}

func (s *Store) ConfirmRestoredPricingWithAuditIntent(ctx context.Context, deploymentID string, intent domain.PricingAuditIntent) error {
	if err := intent.Validate(); err != nil {
		return err
	}
	if intent.Action != "deployment_price.restore_confirm" || intent.TargetID != deploymentID {
		return errors.New("pricing audit intent does not match restore confirmation")
	}
	return s.confirmRestoredPricing(ctx, deploymentID, &intent)
}

func (s *Store) CancelDeploymentPriceVersionWithAuditIntent(ctx context.Context, deploymentID, priceID, actor string, cancelledAt time.Time, expectedRevision uint64, intent domain.PricingAuditIntent) (domain.DeploymentPriceVersion, error) {
	if err := intent.Validate(); err != nil {
		return domain.DeploymentPriceVersion{}, err
	}
	if intent.Action != "deployment_price.cancel" || intent.TargetID != priceID || intent.ActorID != strings.TrimSpace(actor) {
		return domain.DeploymentPriceVersion{}, errors.New("pricing audit intent does not match price cancellation")
	}
	return s.cancelDeploymentPriceVersion(ctx, deploymentID, priceID, actor, cancelledAt, expectedRevision, &intent)
}

func (s *Store) ListPendingPricingAuditIntents(ctx context.Context) ([]domain.PricingAuditIntent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var intents []domain.PricingAuditIntent
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketPricingAuditIntents).ForEach(func(_, raw []byte) error {
			if raw == nil {
				return nil
			}
			var intent domain.PricingAuditIntent
			if err := json.Unmarshal(raw, &intent); err != nil {
				return err
			}
			if err := intent.Validate(); err != nil {
				return err
			}
			if !intent.Delivered {
				intents = append(intents, intent)
			}
			return nil
		})
	})
	sort.Slice(intents, func(i, j int) bool {
		if intents[i].OccurredAt.Equal(intents[j].OccurredAt) {
			return intents[i].EventID < intents[j].EventID
		}
		return intents[i].OccurredAt.Before(intents[j].OccurredAt)
	})
	return intents, err
}

func (s *Store) MarkPricingAuditIntentDelivered(ctx context.Context, eventID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketPricingAuditIntents)
		raw := bucket.Get([]byte(eventID))
		if raw == nil {
			return ErrNotFound
		}
		var intent domain.PricingAuditIntent
		if err := json.Unmarshal(raw, &intent); err != nil {
			return err
		}
		if intent.Delivered {
			return nil
		}
		intent.Delivered = true
		encoded, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(eventID), encoded)
	})
}

func putPricingAuditIntentTx(tx *bbolt.Tx, intent domain.PricingAuditIntent) error {
	if err := intent.Validate(); err != nil {
		return err
	}
	bucket := tx.Bucket(bucketPricingAuditIntents)
	if raw := bucket.Get([]byte(intent.EventID)); raw != nil {
		var existing domain.PricingAuditIntent
		if err := json.Unmarshal(raw, &existing); err != nil {
			return err
		}
		if existing != intent {
			return errors.New("pricing audit event id conflicts with another intent")
		}
		return nil
	}
	encoded, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	return bucket.Put([]byte(intent.EventID), encoded)
}
