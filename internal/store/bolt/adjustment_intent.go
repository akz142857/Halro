package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

type CostAdjustmentIntent struct {
	LedgerEvent    ledger.Event `json:"ledger_event"`
	AuditEventID   string       `json:"audit_event_id"`
	ActorID        string       `json:"actor_id"`
	CorrelationID  string       `json:"correlation_id,omitempty"`
	CreatedAt      time.Time    `json:"created_at"`
	Delivered      bool         `json:"delivered"`
	Rejected       bool         `json:"rejected,omitempty"`
	RejectedReason string       `json:"rejected_reason,omitempty"`
}

func (i CostAdjustmentIntent) Validate() error {
	if i.LedgerEvent.Kind != ledger.EventCostAdjusted || i.AuditEventID == "" || i.ActorID == "" || i.CreatedAt.IsZero() ||
		i.ActorID != i.LedgerEvent.AdjustmentCreatedBy || !domain.ValidSHA256Label(i.LedgerEvent.AdjustmentRequestDigest) {
		return errors.New("invalid cost adjustment intent")
	}
	return i.LedgerEvent.Validate()
}

func (s *Store) PutCostAdjustmentIntent(ctx context.Context, intent CostAdjustmentIntent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := intent.Validate(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketCostAdjustmentIntents)
		key := []byte(intent.LedgerEvent.EventID)
		if raw := bucket.Get(key); raw != nil {
			var existing CostAdjustmentIntent
			if err := json.Unmarshal(raw, &existing); err != nil {
				return err
			}
			left, _ := json.Marshal(existing)
			right, _ := json.Marshal(intent)
			if string(left) != string(right) {
				return ErrIdempotencyConflict
			}
			return nil
		}
		encoded, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		return bucket.Put(key, encoded)
	})
}

func (s *Store) ListPendingCostAdjustmentIntents(ctx context.Context) ([]CostAdjustmentIntent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var result []CostAdjustmentIntent
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketCostAdjustmentIntents).ForEach(func(_, raw []byte) error {
			if raw == nil {
				return nil
			}
			var intent CostAdjustmentIntent
			if err := json.Unmarshal(raw, &intent); err != nil {
				return err
			}
			if err := intent.Validate(); err != nil {
				return err
			}
			if !intent.Delivered && !intent.Rejected {
				result = append(result, intent)
			}
			return nil
		})
	})
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].LedgerEvent.EventID < result[j].LedgerEvent.EventID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, err
}

func (s *Store) MarkCostAdjustmentIntentRejected(ctx context.Context, ledgerEventID, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if reason == "" {
		return errors.New("rejection reason is required")
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketCostAdjustmentIntents)
		raw := bucket.Get([]byte(ledgerEventID))
		if raw == nil {
			return ErrNotFound
		}
		var intent CostAdjustmentIntent
		if err := json.Unmarshal(raw, &intent); err != nil {
			return err
		}
		intent.Rejected, intent.RejectedReason = true, reason
		encoded, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(ledgerEventID), encoded)
	})
}

func (s *Store) MarkCostAdjustmentIntentDelivered(ctx context.Context, ledgerEventID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketCostAdjustmentIntents)
		raw := bucket.Get([]byte(ledgerEventID))
		if raw == nil {
			return ErrNotFound
		}
		var intent CostAdjustmentIntent
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
		return bucket.Put([]byte(ledgerEventID), encoded)
	})
}
