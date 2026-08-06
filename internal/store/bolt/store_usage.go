package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

func (s *Store) PutUsageCheckpoint(watermark ledger.Watermark, payload []byte) error {
	if watermark.Sequence == 0 || watermark.Offset <= 0 || watermark.Generation != 1 {
		return errors.New("usage checkpoint watermark is invalid")
	}
	if len(payload) == 0 {
		return errors.New("usage checkpoint payload cannot be empty")
	}
	encoded, err := json.Marshal(usageCheckpoint{Watermark: watermark, Payload: bytes.Clone(payload)})
	if err != nil {
		return fmt.Errorf("encode usage checkpoint envelope: %w", err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if current := meta.Get(keyUsageCheckpoint); current != nil {
			var saved usageCheckpoint
			if err := json.Unmarshal(current, &saved); err != nil {
				return fmt.Errorf("decode current usage checkpoint: %w", err)
			}
			if watermark.Sequence < saved.Watermark.Sequence {
				return errors.New("usage checkpoint watermark cannot move backwards")
			}
		}
		return meta.Put(keyUsageCheckpoint, encoded)
	})
}

func (s *Store) UsageCheckpoint() (ledger.Watermark, []byte, error) {
	var saved usageCheckpoint
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyUsageCheckpoint)
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &saved); err != nil {
			return fmt.Errorf("decode usage checkpoint envelope: %w", err)
		}
		return nil
	})
	if err != nil {
		return ledger.Watermark{}, nil, err
	}
	if saved.Watermark.Sequence == 0 || saved.Watermark.Offset <= 0 ||
		saved.Watermark.Generation != 1 || len(saved.Payload) == 0 {
		return ledger.Watermark{}, nil, errors.New("usage checkpoint is invalid")
	}
	return saved.Watermark, bytes.Clone(saved.Payload), nil
}

// DeleteUsageCheckpoint removes only the rebuildable aggregate accelerator.
// The Ledger remains authoritative and the next startup replays it from zero.
func (s *Store) DeleteUsageCheckpoint() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMeta).Delete(keyUsageCheckpoint)
	})
}

func (s *Store) PutTokenGuardCheckpoint(payload []byte) error {
	if len(payload) == 0 || len(payload) > 64<<20 {
		return errors.New("Token Guard checkpoint payload is invalid")
	}
	copyPayload := bytes.Clone(payload)
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(keyTokenGuardCheckpoint, copyPayload)
	})
}

func (s *Store) TokenGuardCheckpoint() ([]byte, error) {
	var payload []byte
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyTokenGuardCheckpoint)
		if raw == nil {
			return ErrNotFound
		}
		payload = bytes.Clone(raw)
		return nil
	})
	return payload, err
}

func (s *Store) DeleteTokenGuardCheckpoint() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMeta).Delete(keyTokenGuardCheckpoint)
	})
}

func (s *Store) PutRedactionPolicy(
	ctx context.Context,
	policy domain.RedactionPolicy,
	expectedRevision uint64,
) (domain.RedactionPolicy, error) {
	if err := policy.Validate(); err != nil {
		return domain.RedactionPolicy{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.RedactionPolicy{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		return putVersioned(
			tx.Bucket(bucketRedactionPolicies),
			policy.ID,
			expectedRevision,
			&policy,
		)
	})
	return policy, err
}

func (s *Store) GetRedactionPolicy(ctx context.Context, id string) (domain.RedactionPolicy, error) {
	var policy domain.RedactionPolicy
	err := s.getJSON(ctx, bucketRedactionPolicies, id, &policy)
	return policy, err
}

func (s *Store) ListRedactionPolicies(ctx context.Context) ([]domain.RedactionPolicy, error) {
	var policies []domain.RedactionPolicy
	err := s.listJSON(ctx, bucketRedactionPolicies, func(raw []byte) error {
		var policy domain.RedactionPolicy
		if err := json.Unmarshal(raw, &policy); err != nil {
			return err
		}
		policies = append(policies, policy)
		return nil
	})
	sort.Slice(policies, func(i, j int) bool { return policies[i].ID < policies[j].ID })
	return policies, err
}

func (s *Store) PutTokenGuardPolicy(
	ctx context.Context,
	policy domain.TokenGuardPolicy,
	expectedRevision uint64,
) (domain.TokenGuardPolicy, error) {
	if err := policy.Validate(); err != nil {
		return domain.TokenGuardPolicy{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.TokenGuardPolicy{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		return putVersioned(tx.Bucket(bucketTokenGuardPolicies), policy.ID, expectedRevision, &policy)
	})
	return policy, err
}

func (s *Store) GetTokenGuardPolicy(ctx context.Context, id string) (domain.TokenGuardPolicy, error) {
	var policy domain.TokenGuardPolicy
	err := s.getJSON(ctx, bucketTokenGuardPolicies, id, &policy)
	return policy, err
}

func (s *Store) ListTokenGuardPolicies(ctx context.Context) ([]domain.TokenGuardPolicy, error) {
	var policies []domain.TokenGuardPolicy
	err := s.listJSON(ctx, bucketTokenGuardPolicies, func(raw []byte) error {
		var policy domain.TokenGuardPolicy
		if err := json.Unmarshal(raw, &policy); err != nil {
			return err
		}
		policies = append(policies, policy)
		return nil
	})
	sort.Slice(policies, func(i, j int) bool { return policies[i].ID < policies[j].ID })
	return policies, err
}

func (s *Store) PutAlertWebhook(
	ctx context.Context,
	webhook domain.AlertWebhook,
	expectedRevision uint64,
) (domain.AlertWebhook, error) {
	if err := webhook.Validate(); err != nil {
		return domain.AlertWebhook{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.AlertWebhook{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if webhook.CredentialID != "" &&
			tx.Bucket(bucketCredentials).Get([]byte(webhook.CredentialID)) == nil {
			return fmt.Errorf("credential %q: %w", webhook.CredentialID, ErrNotFound)
		}
		return putVersioned(tx.Bucket(bucketAlertWebhooks), webhook.ID, expectedRevision, &webhook)
	})
	return webhook, err
}

func (s *Store) PutAlertWebhookBundle(
	ctx context.Context,
	webhook domain.AlertWebhook,
	expectedWebhookRevision uint64,
	credential *domain.Credential,
	expectedCredentialRevision uint64,
	deleteCredentialID string,
) (domain.AlertWebhook, error) {
	if err := webhook.Validate(); err != nil {
		return domain.AlertWebhook{}, err
	}
	if credential != nil {
		if err := credential.Validate(); err != nil {
			return domain.AlertWebhook{}, err
		}
		if webhook.CredentialID == "" || credential.ID != webhook.CredentialID {
			return domain.AlertWebhook{}, errors.New("alert credential does not match webhook")
		}
	}
	if deleteCredentialID != "" && deleteCredentialID == webhook.CredentialID {
		return domain.AlertWebhook{}, errors.New("cannot delete the active alert credential")
	}
	if err := ctx.Err(); err != nil {
		return domain.AlertWebhook{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if credential != nil {
			if err := putVersioned(
				tx.Bucket(bucketCredentials),
				credential.ID,
				expectedCredentialRevision,
				credential,
			); err != nil {
				return err
			}
		}
		if webhook.CredentialID != "" &&
			tx.Bucket(bucketCredentials).Get([]byte(webhook.CredentialID)) == nil {
			return fmt.Errorf("credential %q: %w", webhook.CredentialID, ErrNotFound)
		}
		if err := putVersioned(
			tx.Bucket(bucketAlertWebhooks),
			webhook.ID,
			expectedWebhookRevision,
			&webhook,
		); err != nil {
			return err
		}
		if deleteCredentialID == "" {
			return nil
		}
		if tx.Bucket(bucketCredentials).Get([]byte(deleteCredentialID)) == nil {
			return ErrNotFound
		}
		if err := ensureCredentialUnreferenced(tx, deleteCredentialID); err != nil {
			return err
		}
		return tx.Bucket(bucketCredentials).Delete([]byte(deleteCredentialID))
	})
	return webhook, err
}

func (s *Store) GetAlertWebhook(ctx context.Context, id string) (domain.AlertWebhook, error) {
	var webhook domain.AlertWebhook
	err := s.getJSON(ctx, bucketAlertWebhooks, id, &webhook)
	return webhook, err
}

func (s *Store) ListAlertWebhooks(ctx context.Context) ([]domain.AlertWebhook, error) {
	var webhooks []domain.AlertWebhook
	err := s.listJSON(ctx, bucketAlertWebhooks, func(raw []byte) error {
		var webhook domain.AlertWebhook
		if err := json.Unmarshal(raw, &webhook); err != nil {
			return err
		}
		webhooks = append(webhooks, webhook)
		return nil
	})
	sort.Slice(webhooks, func(i, j int) bool { return webhooks[i].ID < webhooks[j].ID })
	return webhooks, err
}
