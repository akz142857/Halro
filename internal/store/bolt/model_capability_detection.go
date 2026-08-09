package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

type capabilityDetectionIdempotency struct {
	RequestHash string `json:"request_hash"`
	DetectionID string `json:"detection_id"`
}

// CreateModelCapabilityDetection atomically installs the job and both lookup
// indexes. It is the single-flight and idempotency serialization point across
// browser tabs and concurrent HTTP handlers.
func (s *Store) CreateModelCapabilityDetection(ctx context.Context, detection domain.ModelCapabilityDetection) (domain.ModelCapabilityDetection, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.ModelCapabilityDetection{}, false, err
	}
	var replayed bool
	err := s.db.Update(func(tx *bbolt.Tx) error {
		idem := tx.Bucket(bucketCapabilityDetectionIdem)
		if raw := idem.Get([]byte(detection.IdempotencyKeyHash)); raw != nil {
			var record capabilityDetectionIdempotency
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			if record.RequestHash != detection.RequestHash {
				return ErrIdempotencyConflict
			}
			rawDetection := tx.Bucket(bucketModelCapabilityDetections).Get([]byte(record.DetectionID))
			if rawDetection == nil {
				return errors.New("capability detection idempotency record is orphaned")
			}
			if err := json.Unmarshal(rawDetection, &detection); err != nil {
				return err
			}
			replayed = true
			return nil
		}
		index := tx.Bucket(bucketCapabilityDetectionIndex)
		if existingID := index.Get([]byte(detection.TargetFingerprint)); existingID != nil {
			if raw := tx.Bucket(bucketModelCapabilityDetections).Get(existingID); raw != nil {
				var existing domain.ModelCapabilityDetection
				if err := json.Unmarshal(raw, &existing); err != nil {
					return err
				}
				if existing.Status == domain.DetectionQueued || existing.Status == domain.DetectionRunning || (!detection.ForceRefresh && existing.Fresh(time.Now().UTC())) {
					record, err := json.Marshal(capabilityDetectionIdempotency{RequestHash: detection.RequestHash, DetectionID: existing.ID})
					if err != nil {
						return err
					}
					if err := idem.Put([]byte(detection.IdempotencyKeyHash), record); err != nil {
						return err
					}
					detection, replayed = existing, true
					return nil
				}
			}
		}
		if err := detection.Validate(); err != nil {
			return err
		}
		if err := putVersioned(tx.Bucket(bucketModelCapabilityDetections), detection.ID, 0, &detection); err != nil {
			return err
		}
		record, err := json.Marshal(capabilityDetectionIdempotency{RequestHash: detection.RequestHash, DetectionID: detection.ID})
		if err != nil {
			return err
		}
		if err := idem.Put([]byte(detection.IdempotencyKeyHash), record); err != nil {
			return err
		}
		return index.Put([]byte(detection.TargetFingerprint), []byte(detection.ID))
	})
	return detection, replayed, err
}

func (s *Store) GetModelCapabilityDetection(ctx context.Context, id string) (domain.ModelCapabilityDetection, error) {
	var detection domain.ModelCapabilityDetection
	err := s.getJSON(ctx, bucketModelCapabilityDetections, id, &detection)
	return detection, err
}

func (s *Store) PutModelCapabilityDetection(ctx context.Context, detection domain.ModelCapabilityDetection, expected uint64) (domain.ModelCapabilityDetection, error) {
	if err := ctx.Err(); err != nil {
		return domain.ModelCapabilityDetection{}, err
	}
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if err := detection.Validate(); err != nil {
			return err
		}
		return putVersioned(tx.Bucket(bucketModelCapabilityDetections), detection.ID, expected, &detection)
	})
	return detection, err
}

func (s *Store) ListModelCapabilityDetections(ctx context.Context) ([]domain.ModelCapabilityDetection, error) {
	items := []domain.ModelCapabilityDetection{}
	err := s.listJSON(ctx, bucketModelCapabilityDetections, func(raw []byte) error {
		var item domain.ModelCapabilityDetection
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		items = append(items, item)
		return nil
	})
	return items, err
}

func (s *Store) DeleteModelCapabilityDetection(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketModelCapabilityDetections)
		raw := bucket.Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		var d domain.ModelCapabilityDetection
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
		if err := bucket.Delete([]byte(id)); err != nil {
			return err
		}
		idem := tx.Bucket(bucketCapabilityDetectionIdem)
		var idempotencyKeys [][]byte
		if err := idem.ForEach(func(key, raw []byte) error {
			var record capabilityDetectionIdempotency
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			if record.DetectionID == id {
				idempotencyKeys = append(idempotencyKeys, append([]byte(nil), key...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, key := range idempotencyKeys {
			if err := idem.Delete(key); err != nil {
				return err
			}
		}
		index := tx.Bucket(bucketCapabilityDetectionIndex)
		if string(index.Get([]byte(d.TargetFingerprint))) == id {
			return index.Delete([]byte(d.TargetFingerprint))
		}
		return nil
	})
}

// InterruptModelCapabilityDetections turns in-flight calls into UNKNOWN during
// startup. Possibly billable calls are never replayed after an ambiguous crash.
func (s *Store) InterruptModelCapabilityDetections(ctx context.Context, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	count := 0
	err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketModelCapabilityDetections)
		return bucket.ForEach(func(key, raw []byte) error {
			var d domain.ModelCapabilityDetection
			if err := json.Unmarshal(raw, &d); err != nil {
				return err
			}
			if d.Status != domain.DetectionQueued && d.Status != domain.DetectionRunning {
				return nil
			}
			for i := range d.Calls {
				if d.Calls[i].Status == "reserved" || d.Calls[i].Status == "running" {
					d.Calls[i].Status, d.Calls[i].FinishedAt = "unknown", &now
				}
			}
			d.Status, d.CompletedAt, d.UpdatedAt = domain.DetectionInterrupted, &now, now
			for capability, result := range d.Results {
				if result.CompletedAt == nil {
					result.Status, result.CompletedAt = domain.ProbeInconclusive, &now
					d.Results[capability] = result
				}
			}
			d.Revision++
			encoded, err := json.Marshal(d)
			if err != nil {
				return err
			}
			if err := bucket.Put(key, encoded); err != nil {
				return err
			}
			count++
			return nil
		})
	})
	return count, err
}
