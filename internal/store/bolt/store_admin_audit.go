package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

// maxAdminAuditIntents bounds how many undelivered admin audit intents may
// accumulate before mutations are refused.
//
// Reaching it means delivery has been failing for a long time, which is the
// state where continuing to accept admin mutations would keep producing
// changes whose audit records are not in the log. Refusing is the fail-closed
// side: the operator is told the audit trail is behind rather than quietly
// building a backlog nothing is draining.
const maxAdminAuditIntents = 4096

// putAdminAuditIntentTx makes an admin mutation's audit record durable inside
// the transaction that performs the mutation.
//
// The caller passes nil when the mutation is not operator-initiated — startup
// reconciliation and internal maintenance write the same records through paths
// that already have their own audit story.
func putAdminAuditIntentTx(tx *bbolt.Tx, intent *domain.AdminAuditIntent) error {
	if intent == nil {
		return nil
	}
	if err := intent.Validate(); err != nil {
		return err
	}
	bucket := tx.Bucket(bucketAdminAuditIntents)
	if bucket == nil {
		return errors.New("admin audit intents bucket is missing")
	}
	if raw := bucket.Get([]byte(intent.EventID)); raw != nil {
		return errors.New("admin audit event id conflicts with another intent")
	}
	if bucket.Stats().KeyN >= maxAdminAuditIntents {
		return errors.New("admin audit intent backlog is full: the audit log is not accepting deliveries")
	}
	encoded, err := json.Marshal(*intent)
	if err != nil {
		return err
	}
	return bucket.Put([]byte(intent.EventID), encoded)
}

// ListPendingAdminAuditIntents returns the admin audit records that are durable
// but not yet in the audit log, oldest first.
//
// Every row in the bucket is pending by construction: delivery removes what it
// appended. Ordering is by occurrence so a drain reproduces the order the
// mutations happened in.
func (s *Store) ListPendingAdminAuditIntents(ctx context.Context) ([]domain.AdminAuditIntent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var intents []domain.AdminAuditIntent
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketAdminAuditIntents).ForEach(func(_, raw []byte) error {
			if raw == nil {
				return nil
			}
			var intent domain.AdminAuditIntent
			if err := json.Unmarshal(raw, &intent); err != nil {
				return err
			}
			if err := intent.Validate(); err != nil {
				return err
			}
			intents = append(intents, intent)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(intents, func(i, j int) bool {
		if intents[i].OccurredAt.Equal(intents[j].OccurredAt) {
			return intents[i].EventID < intents[j].EventID
		}
		return intents[i].OccurredAt.Before(intents[j].OccurredAt)
	})
	return intents, nil
}

// DeleteAdminAuditIntent retires an intent whose audit record is durable.
//
// It is called only after the append and the audit checkpoint have both
// succeeded, so a missing row means another drain already finished the same
// work rather than that anything was lost.
func (s *Store) DeleteAdminAuditIntent(ctx context.Context, eventID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketAdminAuditIntents).Delete([]byte(eventID))
	})
}

// PendingAdminAuditIntentCount reports the backlog size for status and doctor.
func (s *Store) PendingAdminAuditIntentCount(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	count := 0
	err := s.db.View(func(tx *bbolt.Tx) error {
		count = tx.Bucket(bucketAdminAuditIntents).Stats().KeyN
		return nil
	})
	return count, err
}
