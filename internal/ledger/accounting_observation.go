package ledger

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"time"

	bbolt "go.etcd.io/bbolt"
)

// AccountingObservationReader exposes pending facts from one accounting cut.
// A failed read is not evidence of zero pending work. Lifetime and trust are
// established by its owner, not by the presence of these derived indexes.
type AccountingObservationReader interface {
	ReadPendingLeaseStats(context.Context, time.Time) (PendingLeaseStatistics, error)
}

type PendingLeaseStatistics struct {
	Count     int64
	OldestAge time.Duration
	Watermark Watermark
}

var _ AccountingObservationReader = (*State)(nil)
var _ AccountingObservationReader = (*LedgerProjection)(nil)

func (s *State) ReadPendingLeaseStats(ctx context.Context, now time.Time) (PendingLeaseStatistics, error) {
	if err := ctx.Err(); err != nil {
		return PendingLeaseStatistics{}, err
	}
	if s == nil {
		return PendingLeaseStatistics{}, ErrProjectionUnavailable
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var oldest time.Time
	for _, reservation := range s.reservations {
		if err := ctx.Err(); err != nil {
			return PendingLeaseStatistics{}, err
		}
		if oldest.IsZero() || reservation.Lease.OccurredAt.Before(oldest) {
			oldest = reservation.Lease.OccurredAt
		}
	}
	result := PendingLeaseStatistics{Count: int64(len(s.reservations)), Watermark: s.watermark}
	if !oldest.IsZero() && !now.Before(oldest) {
		result.OldestAge = now.Sub(oldest)
	}
	if err := ctx.Err(); err != nil {
		return PendingLeaseStatistics{}, err
	}
	return result, nil
}

// Signed Unix seconds and nanoseconds cover the native time range. UnixNano
// would reorder dates outside its range. Hashing the ID bounds bbolt key size.
func projectionPendingTimeKey(e Event) []byte {
	key := binary.BigEndian.AppendUint64(nil, uint64(e.OccurredAt.Unix())^(uint64(1)<<63))
	key = binary.BigEndian.AppendUint32(key, uint32(e.OccurredAt.Nanosecond()))
	return append(key, projectionIDKey(e.AttemptID)...)
}

func projectionUpdatePendingStatistics(tx projectionRows, e Event) error {
	if e.Kind != EventReservationCreated && e.Kind != EventAttemptSettled {
		return nil
	}
	count, found, err := projectionRowsGet[int64](tx, "meta", []byte("pending_count"))
	if err != nil || !found || count < 0 {
		return errors.Join(ErrProjectionCorrupt, err)
	}
	position, found, err := projectionRowsGet[projectionPosition](tx, "meta", []byte("position"))
	if err != nil || !found || uint64(count) > position.Watermark.Sequence {
		return errors.Join(ErrProjectionCorrupt, err)
	}
	if e.Kind == EventReservationCreated {
		_, exists, err := tx.get("pending_by_time", projectionPendingTimeKey(e))
		if err != nil {
			return err
		}
		if count == math.MaxInt64 || exists {
			return ErrProjectionCorrupt
		}
		if err := tx.put("pending_by_time", projectionPendingTimeKey(e), []byte(e.AttemptID)); err != nil {
			return err
		}
		count++
	} else {
		// Use the original reservation's timestamp, not settlement time.
		pending, found, err := projectionRowsGet[projectionReservation](tx, "pending", projectionIDKey(e.AttemptID))
		if err != nil || !found || count == 0 {
			return errors.Join(ErrProjectionCorrupt, err)
		}
		reservation := pending.Evidence.Event
		raw, exists, err := tx.get("pending_by_time", projectionPendingTimeKey(reservation))
		if err != nil {
			return err
		}
		if reservation.AttemptID != e.AttemptID || reservation.Kind != EventReservationCreated || !exists || !bytes.Equal(raw, []byte(e.AttemptID)) {
			return ErrProjectionCorrupt
		}
		if err := tx.delete("pending_by_time", projectionPendingTimeKey(reservation)); err != nil {
			return err
		}
		count--
	}
	return projectionRowsPut(tx, "meta", []byte("pending_count"), count)
}

func (p *LedgerProjection) ReadPendingLeaseStats(ctx context.Context, now time.Time) (PendingLeaseStatistics, error) {
	var result PendingLeaseStatistics
	err := p.governanceCapacityView(ctx, func(tx *bbolt.Tx) error {
		position, found, err := projectionGet[projectionPosition](tx, "meta", []byte("position"))
		if err != nil || !found {
			return errors.Join(ErrProjectionCorrupt, err)
		}
		count, found, err := projectionGet[int64](tx, "meta", []byte("pending_count"))
		if err != nil || !found || count < 0 || uint64(count) > position.Watermark.Sequence {
			return errors.Join(ErrProjectionCorrupt, err)
		}
		b := tx.Bucket([]byte("pending_by_time"))
		if b == nil || tx.Bucket([]byte("pending")) == nil {
			return ErrProjectionCorrupt
		}
		k, rawID := b.Cursor().First()
		if (count == 0) != (k == nil) {
			return ErrProjectionCorrupt
		}
		pendingKey, _ := tx.Bucket([]byte("pending")).Cursor().First()
		if (count == 0) != (pendingKey == nil) {
			return ErrProjectionCorrupt
		}
		result = PendingLeaseStatistics{Count: count, Watermark: position.Watermark}
		if count == 0 {
			return nil
		}
		if len(k) != 44 || len(rawID) == 0 || len(rawID) > maxPayloadSize {
			return ErrProjectionCorrupt
		}
		pending, found, err := projectionGet[projectionReservation](tx, "pending", projectionIDKey(string(rawID)))
		if err != nil {
			return err
		}
		e := pending.Evidence.Event
		if !found || pending.Evidence.Sequence == 0 || pending.Evidence.Sequence > position.Watermark.Sequence || e.Kind != EventReservationCreated || e.AttemptID != string(rawID) || pending.Reservation.Lease.AttemptID != e.AttemptID || !bytes.Equal(k, projectionPendingTimeKey(e)) {
			return ErrProjectionCorrupt
		}
		if err := e.Validate(); err != nil {
			return errors.Join(ErrProjectionCorrupt, err)
		}
		if !now.Before(e.OccurredAt) {
			result.OldestAge = now.Sub(e.OccurredAt)
		}
		return nil
	})
	if err != nil {
		return PendingLeaseStatistics{}, err
	}
	return result, nil
}
