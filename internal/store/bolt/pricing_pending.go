package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

var pricingPendingCountKey = []byte("pending_count")

func pricingPendingRaw(raw []byte) (int64, error) {
	if raw == nil {
		return 0, nil
	}
	if len(raw) > metadatajournal.MaxPayloadSize {
		return 0, ErrPricingAuditLimit
	}
	var intent struct {
		Delivered bool `json:"delivered"`
	}
	if err := json.Unmarshal(raw, &intent); err != nil {
		return 0, err
	}
	if intent.Delivered {
		return 0, nil
	}
	return 1, nil
}

// Migration scans borrowed records one at a time and writes only a fixed
// counter. It creates no history-sized index or dirty-page set. This remains
// an O(history) schema preparation scan, not the final O-01 migration command.
func migratePricingAuditPendingCount(tx *Tx, step func(string) error) error {
	if err := migrationStep(step, "before_pricing_audit_count"); err != nil {
		return err
	}
	b, err := tx.CreateBucketIfNotExists(bucketPricingAuditControl)
	if err != nil {
		return err
	}
	var count uint64
	if err := tx.Bucket(bucketPricingAuditIntents).ForEach(func(_, raw []byte) error {
		if raw == nil {
			return errors.New("pricing audit intents cannot contain child buckets")
		}
		pending, err := pricingPendingRaw(raw)
		if err != nil {
			return err
		}
		if pending != 0 {
			if count == math.MaxUint64 {
				return errors.New("pricing pending count exhausted")
			}
			count++
		}
		return nil
	}); err != nil {
		return err
	}
	if err := b.Put(pricingPendingCountKey, encodeUint64(count)); err != nil {
		return err
	}
	return migrationStep(step, "after_pricing_audit_count")
}

func pricingPendingCountTx(tx *Tx) (uint64, error) {
	b := tx.Bucket(bucketPricingAuditControl)
	if b == nil {
		return 0, errors.New("pricing audit count bucket missing")
	}
	v, ok := decodeUint64(b.Get(pricingPendingCountKey))
	if !ok {
		return 0, errors.New("pricing audit count missing or malformed")
	}
	return v, nil
}

func (r *recorder) trackPricingPendingMutation(kind metadatajournal.OpKind, path []string, previous, value []byte) error {
	if len(path) == 0 {
		return nil
	}
	if path[0] == string(bucketPricingAuditControl) {
		if !r.updatingPricingPending {
			return errors.New("pricing audit count is owned by the transaction entry")
		}
		return nil
	}
	if path[0] != string(bucketPricingAuditIntents) {
		return nil
	}
	if len(path) != 1 || kind == metadatajournal.OpDeleteBucket {
		return errors.New("pricing audit root mutation requires whole-file publication")
	}
	before, err := pricingPendingRaw(previous)
	if err != nil {
		return err
	}
	after, err := pricingPendingRaw(value)
	if err != nil {
		return err
	}
	delta := after - before
	if delta > 0 && r.pricingPendingDelta == math.MaxInt64 || delta < 0 && r.pricingPendingDelta == math.MinInt64 {
		return errors.New("pricing pending delta exhausted")
	}
	r.pricingPendingDelta += delta
	return nil
}

func (tx *Tx) updatePricingPendingCount() error {
	current, err := pricingPendingCountTx(tx)
	if err != nil {
		return err
	}
	delta := tx.recorder.pricingPendingDelta
	if delta == 0 {
		return nil
	}
	if delta > 0 {
		if uint64(delta) > math.MaxUint64-current {
			return errors.New("pricing pending count overflow")
		}
		current += uint64(delta)
	} else {
		decrease := uint64(-(delta + 1)) + 1
		if decrease > current {
			return errors.New("pricing pending count underflow")
		}
		current -= decrease
	}
	tx.recorder.updatingPricingPending = true
	defer func() { tx.recorder.updatingPricingPending = false }()
	return tx.Bucket(bucketPricingAuditControl).Put(pricingPendingCountKey, encodeUint64(current))
}

func (s *Store) PendingPricingAuditIntentCount(ctx context.Context) (count uint64, err error) {
	err = s.view(func(tx *Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := proposalIndexReadyTx(tx); err != nil {
			return err
		}
		count, err = pricingPendingCountTx(tx)
		return err
	})
	return count, err
}

type pricingIntentRow struct {
	id  string
	raw []byte
}

// Copy a finite physical page, including delivered rows so scanning long
// delivered prefixes releases its reader at the same byte/row boundaries.
func (s *Store) pricingIntentPage(ctx context.Context, after, upper []byte) (rows []pricingIntentRow, next []byte, err error) {
	err = s.view(func(tx *Tx) error {
		c := tx.Bucket(bucketPricingAuditIntents).Cursor()
		key, raw := c.First()
		if after != nil {
			key, raw = c.Seek(after)
			if bytes.Equal(key, after) {
				key, raw = c.Next()
			}
		}
		used := 0
		for key != nil && bytes.Compare(key, upper) <= 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			if raw == nil {
				return errors.New("pricing audit intents cannot contain child buckets")
			}
			if len(key) > metadatajournal.MaxPayloadSize || len(raw) > metadatajournal.MaxPayloadSize-len(key) {
				return ErrPricingAuditLimit
			}
			if len(rows) == 64 || len(rows) > 0 && len(raw)+len(key) > priceAuditPageBytes-used {
				next = []byte(rows[len(rows)-1].id)
				break
			}
			rows = append(rows, pricingIntentRow{string(key), bytes.Clone(raw)})
			used += len(raw) + len(key)
			key, raw = c.Next()
		}
		return nil
	})
	return
}

// RecoverPendingPricingAuditIntents is a trusted startup job. It owns one
// Recovery workspace through validation, delivery callbacks and actual exit.
// Callbacks must not synchronously Close this Store. OccurredAt is preserved;
// existing retained receipts are retried before new receipts so finite native
// retry caches cannot evict an interrupted append during this startup job.
// New receipts use stable physical EventID order, not timestamp order. The
// trusted caller must keep new audit appends quiescent through the priority pass;
// retainedReceipt is only a hint: deliver still verifies the full payload.
func (s *Store) RecoverPendingPricingAuditIntents(ctx context.Context, deliver func(context.Context, domain.PricingAuditIntent) error, retainedReceipt func(string) bool) (resultErr error) {
	if deliver == nil {
		return resourcebudget.ErrInvalid
	}
	leave, err := s.enter()
	if err != nil {
		return err
	}
	defer leave()
	ctx, cancel := s.confirmationContext(ctx)
	defer cancel()
	count, err := s.PendingPricingAuditIntentCount(ctx)
	if err != nil {
		return err
	}
	r := &s.pricingResources
	r.mu.Lock()
	r.started = true
	manager, ram := r.manager, r.ioRAM
	r.mu.Unlock()
	var upper []byte
	var initialTx int
	if err := s.view(func(tx *Tx) error {
		key, _ := tx.Bucket(bucketPricingAuditIntents).Cursor().Last()
		upper, initialTx = bytes.Clone(key), tx.ID()
		return ctx.Err()
	}); err != nil {
		return err
	}
	if upper == nil {
		if count != 0 {
			return ErrPricingRecoveryChanged
		}
		return ctx.Err()
	}
	if manager != nil {
		permit, err := manager.TryAcquireRecovery(resourcebudget.Cost{RAM: ram})
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, permit.Release()) }()
	}
	// First complete validation preserves the previous all-before-delivery
	// error boundary. No audit record is appended for a malformed late row.
	for pass := 0; pass < 3; pass++ {
		if pass == 1 && retainedReceipt == nil {
			continue
		}
		var after []byte
		var validated uint64
		for {
			rows, next, err := s.pricingIntentPage(ctx, after, upper)
			if err != nil {
				return err
			}
			for _, row := range rows {
				if err := ctx.Err(); err != nil {
					return err
				}
				var intent domain.PricingAuditIntent
				if err := json.Unmarshal(row.raw, &intent); err != nil {
					return err
				}
				if intent.Delivered {
					continue
				}
				if err := intent.Validate(); err != nil {
					return err
				}
				if intent.EventID != row.id {
					return errors.New("pricing intent key does not match event id")
				}
				validated++
				if pass > 0 {
					if pass == 1 && (retainedReceipt == nil || !retainedReceipt(intent.EventID)) {
						continue
					}
					if err := deliver(ctx, intent); err != nil {
						return err
					}
				}
			}
			if next == nil {
				break
			}
			after = next
		}
		if pass == 0 {
			if validated != count {
				return ErrPricingRecoveryChanged
			}
			if err := s.view(func(tx *Tx) error {
				if tx.ID() != initialTx {
					return ErrPricingRecoveryChanged
				}
				return ctx.Err()
			}); err != nil {
				return err
			}
		}
	}
	remaining, err := s.PendingPricingAuditIntentCount(ctx)
	if err != nil {
		return err
	}
	if remaining != 0 {
		return ErrPricingRecoveryChanged
	}
	return ctx.Err()
}
