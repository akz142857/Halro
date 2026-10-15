package bolt

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.etcd.io/bbolt"
)

// auditAnchorRetention bounds the local ring: emitted roughly every 5
// minutes by default (ADR 0015's default interval), this covers a little
// over three days before the oldest entries are pruned. The retained window
// only needs to outlive the gap between two dead-man pulls — the anchors
// themselves are not the durable record once a witness has them, the
// witness's own copy is.
const auditAnchorRetention = 1000

// AuditAnchor is the local record of one emission (ADR 0015): the audit
// chain's summary at the moment observed, plus enough identity to place it —
// no event payloads, so it is safe to send somewhere with weaker
// confidentiality than the audit log itself.
type AuditAnchor struct {
	Sequence   uint64    `json:"sequence"`
	Records    uint64    `json:"records"`
	LastHash   [32]byte  `json:"last_hash"`
	Bytes      int64     `json:"bytes"`
	InstanceID string    `json:"instance_id"`
	ObservedAt time.Time `json:"observed_at"`
}

func auditAnchorKey(sequence uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, sequence)
	return key
}

// AppendAuditAnchor records a new anchor and prunes the ring to
// auditAnchorRetention entries. The sequence must be exactly one more than
// the last recorded anchor (or 1 for the first) — anchors are a local,
// single-writer sequence, not a value an external actor ever proposes.
func (s *Store) AppendAuditAnchor(anchor AuditAnchor) error {
	if anchor.InstanceID == "" || anchor.ObservedAt.IsZero() {
		return errors.New("audit anchor is incomplete")
	}
	encoded, err := json.Marshal(anchor)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAuditAnchors)
		if bucket == nil {
			return errors.New("audit anchor bucket is missing")
		}
		cursor := bucket.Cursor()
		lastKey, lastValue := cursor.Last()
		if lastKey != nil {
			var last AuditAnchor
			if err := json.Unmarshal(lastValue, &last); err != nil {
				return fmt.Errorf("decode last audit anchor: %w", err)
			}
			if anchor.Sequence != last.Sequence+1 {
				return fmt.Errorf("audit anchor sequence %d does not follow %d", anchor.Sequence, last.Sequence)
			}
		} else if anchor.Sequence != 1 {
			return fmt.Errorf("first audit anchor must be sequence 1, got %d", anchor.Sequence)
		}
		if err := bucket.Put(auditAnchorKey(anchor.Sequence), encoded); err != nil {
			return err
		}
		// Prune by sequence arithmetic, not a live key count: bbolt's
		// Bucket.Stats() walks committed pages and does not reliably reflect
		// Put/Delete calls made earlier in the same transaction, so a
		// count-driven loop here either under- or over-prunes depending on
		// when it observes the change.
		if anchor.Sequence > auditAnchorRetention {
			cutoff := anchor.Sequence - auditAnchorRetention
			// Collect the whole range, then delete it — never Delete() through
			// the cursor that is walking it. Cursor.Delete removes the inode
			// from the leaf node, the elements after it shift down one index,
			// and Next() advances the index anyway: the element that moved into
			// the current slot is stepped over, leaving every second key behind.
			// It bites only when the pruned keys sit on a leaf page the Put
			// above already materialised into a node (the cursor reads the
			// unchanged page otherwise), which is why the ring's ordinary
			// one-key-per-append prune has never shown it.
			//
			// The keys point into the transaction's pages, so they are cloned:
			// deleting rebalances the very memory they refer to.
			var expired [][]byte
			cursor := bucket.Cursor()
			for key, _ := cursor.First(); key != nil && binary.BigEndian.Uint64(key) <= cutoff; key, _ = cursor.Next() {
				expired = append(expired, bytes.Clone(key))
			}
			for _, key := range expired {
				if err := bucket.Delete(key); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// AuditAnchorsSince returns every retained anchor with a sequence greater
// than since, in ascending order — the shape a puller needs to catch up
// incrementally without re-fetching what it already has.
func (s *Store) AuditAnchorsSince(since uint64) ([]AuditAnchor, error) {
	var anchors []AuditAnchor
	err := s.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAuditAnchors)
		if bucket == nil {
			return errors.New("audit anchor bucket is missing")
		}
		cursor := bucket.Cursor()
		for key, value := cursor.Seek(auditAnchorKey(since + 1)); key != nil; key, value = cursor.Next() {
			var anchor AuditAnchor
			if err := json.Unmarshal(value, &anchor); err != nil {
				return fmt.Errorf("decode audit anchor: %w", err)
			}
			anchors = append(anchors, anchor)
		}
		return nil
	})
	return anchors, err
}

// LatestAuditAnchor returns the most recently recorded anchor, or
// ErrNotFound if none has been emitted yet.
func (s *Store) LatestAuditAnchor() (AuditAnchor, error) {
	var anchor AuditAnchor
	found := false
	err := s.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketAuditAnchors)
		if bucket == nil {
			return errors.New("audit anchor bucket is missing")
		}
		key, value := bucket.Cursor().Last()
		if key == nil {
			return nil
		}
		found = true
		return json.Unmarshal(value, &anchor)
	})
	if err != nil {
		return AuditAnchor{}, err
	}
	if !found {
		return AuditAnchor{}, ErrNotFound
	}
	return anchor, nil
}
