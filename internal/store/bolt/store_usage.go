package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

// UsageRollupState is the durable position of the daily rollup: which
// structure version wrote it, and how much of the Ledger it has consumed.
//
// It is written in the same transaction as the Usage checkpoint, so the two
// can only ever describe the same prefix of the WAL. Startup checks them for
// equality rather than ordering: if either is missing or they disagree, the
// rollup is cleared and rebuilt. Accepting "rollup is behind" would mean
// replaying a suffix into rows that already counted it.
type UsageRollupState struct {
	Version   int              `json:"version"`
	Watermark ledger.Watermark `json:"watermark"`
}

// PutUsageCheckpoint durably advances both Usage derivatives at once: the
// aggregate checkpoint and the daily-rollup increment that covers exactly the
// same events.
//
// Both live in one bbolt transaction because a checkpoint that advanced
// without its increment would leave the rollup describing a prefix of the WAL
// nobody can name — the next start would replay from the checkpoint's
// watermark and the events in between would be counted nowhere. The increment
// is applied as a read-modify-write per row: every column is additive, which
// is what lets the incremental path and a full rebuild reach the same numbers.
func (s *Store) PutUsageCheckpoint(
	watermark ledger.Watermark,
	payload []byte,
	rollupVersion int,
	rollup map[string]domain.DailyRollup,
) error {
	if watermark.Sequence == 0 || watermark.Offset <= 0 || watermark.Generation == 0 {
		return errors.New("usage checkpoint watermark is invalid")
	}
	if len(payload) == 0 {
		return errors.New("usage checkpoint payload cannot be empty")
	}
	if rollupVersion <= 0 {
		return errors.New("usage rollup version is required")
	}
	encoded, err := json.Marshal(usageCheckpoint{Watermark: watermark, Payload: bytes.Clone(payload)})
	if err != nil {
		return fmt.Errorf("encode usage checkpoint envelope: %w", err)
	}
	state, err := json.Marshal(UsageRollupState{Version: rollupVersion, Watermark: watermark})
	if err != nil {
		return fmt.Errorf("encode usage rollup state: %w", err)
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
		if err := applyUsageRollupDelta(tx, rollup); err != nil {
			return err
		}
		if err := meta.Put(keyUsageRollupState, state); err != nil {
			return err
		}
		return meta.Put(keyUsageCheckpoint, encoded)
	})
}

// applyUsageRollupDelta folds one increment into the stored rows, keeping each
// accounting day's per-dimension key count bounded.
//
// New values are admitted in ledger order — the earliest sequence that reached
// each row — and everything past the cap is merged into a single
// domain.RollupOtherKey row. Ledger order is what makes the cap deterministic:
// the incremental path sees the day in many increments and a rebuild sees it in
// one, and both admit the same values because both walk the same events in the
// same order. Choosing the largest values instead would need the day to be
// finished, and an accounting day never provably is: a request admitted at
// 23:59 and settled at 00:02 is charged to the day it was admitted on.
func applyUsageRollupDelta(tx *bbolt.Tx, rollup map[string]domain.DailyRollup) error {
	if len(rollup) == 0 {
		return nil
	}
	bucket := tx.Bucket(bucketUsageDailyRollup)
	if bucket == nil {
		return errors.New("usage rollup bucket is missing")
	}
	type pending struct {
		key domain.RollupKey
		row domain.DailyRollup
	}
	groups := map[string][]pending{}
	for encoded, row := range rollup {
		parsed, err := domain.DecodeRollupKey(encoded)
		if err != nil {
			return err
		}
		if parsed.PeriodID != row.PeriodID || parsed.TimezoneVersion != row.TimezoneVersion {
			return fmt.Errorf("usage rollup row %q does not match its key", encoded)
		}
		prefix := domain.RollupDimensionPrefix(parsed.PeriodID, parsed.TimezoneVersion, parsed.Dimension)
		groups[prefix] = append(groups[prefix], pending{key: parsed, row: row})
	}
	prefixes := make([]string, 0, len(groups))
	for prefix := range groups {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)

	for _, prefix := range prefixes {
		admitted, err := storedDimensionKeys(bucket, prefix)
		if err != nil {
			return err
		}
		items := groups[prefix]
		sort.Slice(items, func(i, j int) bool {
			if items[i].row.FirstSequence != items[j].row.FirstSequence {
				return items[i].row.FirstSequence < items[j].row.FirstSequence
			}
			return items[i].key.DimensionKey < items[j].key.DimensionKey
		})
		for _, item := range items {
			key := item.key
			if key.Dimension != domain.RollupDimensionTotal && key.DimensionKey != domain.RollupOtherKey {
				if _, known := admitted[key.DimensionKey]; !known {
					if len(admitted) >= domain.MaxRollupKeysPerDimension {
						key.DimensionKey = domain.RollupOtherKey
					} else {
						admitted[key.DimensionKey] = struct{}{}
					}
				}
			}
			if err := mergeRollupRow(bucket, key, item.row); err != nil {
				return err
			}
		}
	}
	return nil
}

// storedDimensionKeys reports which values one day's one dimension already
// holds. RollupOtherKey is excluded: it is the overflow row, not one of the
// values the cap counts.
func storedDimensionKeys(bucket *bbolt.Bucket, prefix string) (map[string]struct{}, error) {
	keys := map[string]struct{}{}
	cursor := bucket.Cursor()
	seek := []byte(prefix)
	for key, _ := cursor.Seek(seek); key != nil && bytes.HasPrefix(key, seek); key, _ = cursor.Next() {
		value := string(key[len(prefix):])
		if value == domain.RollupOtherKey {
			continue
		}
		keys[value] = struct{}{}
	}
	return keys, nil
}

func mergeRollupRow(bucket *bbolt.Bucket, key domain.RollupKey, increment domain.DailyRollup) error {
	encoded := []byte(key.Encode())
	row := increment
	if existing := bucket.Get(encoded); existing != nil {
		var stored domain.DailyRollup
		if err := json.Unmarshal(existing, &stored); err != nil {
			return fmt.Errorf("decode usage rollup row: %w", err)
		}
		if err := stored.Add(increment); err != nil {
			return err
		}
		row = stored
	}
	payload, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("encode usage rollup row: %w", err)
	}
	return bucket.Put(encoded, payload)
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
		saved.Watermark.Generation == 0 || len(saved.Payload) == 0 {
		return ledger.Watermark{}, nil, errors.New("usage checkpoint is invalid")
	}
	return saved.Watermark, bytes.Clone(saved.Payload), nil
}

// UsageRollupState reports where the stored rollup stands.
func (s *Store) UsageRollupState() (UsageRollupState, error) {
	var state UsageRollupState
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyUsageRollupState)
		if raw == nil {
			return ErrNotFound
		}
		return json.Unmarshal(raw, &state)
	})
	if err != nil {
		return UsageRollupState{}, err
	}
	return state, nil
}

// ResetUsageDerivatives discards both rebuildable Usage views together: the
// aggregate checkpoint and every rollup row.
//
// They are dropped in one transaction on purpose. Clearing only the checkpoint
// would leave a complete rollup on disk while the next start replays the whole
// WAL from zero — every row counted twice, with nothing in the logs to say so.
// The Ledger remains authoritative; the next start rebuilds both from it.
func (s *Store) ResetUsageDerivatives() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if err := meta.Delete(keyUsageCheckpoint); err != nil {
			return err
		}
		if err := meta.Delete(keyUsageRollupState); err != nil {
			return err
		}
		if tx.Bucket(bucketUsageDailyRollup) != nil {
			if err := tx.DeleteBucket(bucketUsageDailyRollup); err != nil {
				return err
			}
		}
		_, err := tx.CreateBucketIfNotExists(bucketUsageDailyRollup)
		return err
	})
}

// UsageRollupRange walks the stored rows whose accounting date falls in
// [startPeriodID, endPeriodID], in key order, and hands the visitor a decoded
// copy. An empty bound is open on that side.
//
// The range is over the date label rather than a key prefix: a prefix scan can
// only ever answer for one day, and every question this feeds — a month, a
// year, a custom window — spans many. Period ids are fixed-width local dates,
// so lexicographic key order is chronological and the walk can stop at the
// first row past the end instead of reading the whole bucket.
func (s *Store) UsageRollupRange(
	startPeriodID, endPeriodID string,
	visit func(domain.RollupKey, domain.DailyRollup) error,
) error {
	return s.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketUsageDailyRollup)
		if bucket == nil {
			return errors.New("usage rollup bucket is missing")
		}
		cursor := bucket.Cursor()
		key, value := cursor.Seek([]byte(startPeriodID))
		for ; key != nil; key, value = cursor.Next() {
			parsed, err := domain.DecodeRollupKey(string(key))
			if err != nil {
				return err
			}
			if endPeriodID != "" && parsed.PeriodID > endPeriodID {
				return nil
			}
			var row domain.DailyRollup
			if err := json.Unmarshal(value, &row); err != nil {
				return fmt.Errorf("decode usage rollup row: %w", err)
			}
			if err := visit(parsed, row); err != nil {
				return err
			}
		}
		return nil
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
	intent *domain.AdminAuditIntent,
) (domain.RedactionPolicy, error) {
	if err := policy.Validate(); err != nil {
		return domain.RedactionPolicy{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.RedactionPolicy{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if err := putVersioned(
			tx.Bucket(bucketRedactionPolicies),
			policy.ID,
			expectedRevision,
			&policy,
		); err != nil {
			return err
		}
		return putAdminAuditIntentTx(tx, intent)
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
	intent *domain.AdminAuditIntent,
) (domain.TokenGuardPolicy, error) {
	if err := policy.Validate(); err != nil {
		return domain.TokenGuardPolicy{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.TokenGuardPolicy{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if err := putVersioned(tx.Bucket(bucketTokenGuardPolicies), policy.ID, expectedRevision, &policy); err != nil {
			return err
		}
		return putAdminAuditIntentTx(tx, intent)
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
	intent *domain.AdminAuditIntent,
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
		if err := putVersioned(tx.Bucket(bucketAlertWebhooks), webhook.ID, expectedRevision, &webhook); err != nil {
			return err
		}
		return putAdminAuditIntentTx(tx, intent)
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
	intent *domain.AdminAuditIntent,
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
			return putAdminAuditIntentTx(tx, intent)
		}
		if tx.Bucket(bucketCredentials).Get([]byte(deleteCredentialID)) == nil {
			return ErrNotFound
		}
		if err := ensureCredentialUnreferenced(tx, deleteCredentialID); err != nil {
			return err
		}
		if err := tx.Bucket(bucketCredentials).Delete([]byte(deleteCredentialID)); err != nil {
			return err
		}
		return putAdminAuditIntentTx(tx, intent)
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
