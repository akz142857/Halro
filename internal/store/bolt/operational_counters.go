package bolt

import (
	"encoding/binary"
	"errors"
	"math"

	bbolt "go.etcd.io/bbolt"
)

var keyShutdownTruncatedAttempts = []byte("shutdown_truncated_attempts_total")

// ShutdownTruncatedAttempts returns the durable number of Provider attempts
// that were still active when a graceful-shutdown budget expired. Keeping this
// counter in metadata makes the terminal event visible after the next start;
// an in-memory-only counter would disappear with the process that recorded it.
func (s *Store) ShutdownTruncatedAttempts() (uint64, error) {
	var total uint64
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyShutdownTruncatedAttempts)
		if len(raw) == 0 {
			return nil
		}
		if len(raw) != 8 {
			return errors.New("shutdown truncated-attempt counter is invalid")
		}
		total = binary.BigEndian.Uint64(raw)
		return nil
	})
	return total, err
}

// AddShutdownTruncatedAttempts durably increments the terminal shutdown
// counter and returns the new total.
func (s *Store) AddShutdownTruncatedAttempts(delta uint64) (uint64, error) {
	if delta == 0 {
		return s.ShutdownTruncatedAttempts()
	}
	var total uint64
	err := s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		raw := meta.Get(keyShutdownTruncatedAttempts)
		if len(raw) != 0 && len(raw) != 8 {
			return errors.New("shutdown truncated-attempt counter is invalid")
		}
		if len(raw) == 8 {
			total = binary.BigEndian.Uint64(raw)
		}
		if delta > math.MaxUint64-total {
			return errors.New("shutdown truncated-attempt counter overflow")
		}
		total += delta
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], total)
		return meta.Put(keyShutdownTruncatedAttempts, encoded[:])
	})
	return total, err
}
