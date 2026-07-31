// Package idempotency provides a bounded durable request lifecycle store.
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	bbolt "go.etcd.io/bbolt"
)

type State string

const (
	Reserved   State = "reserved"
	InProgress State = "in_progress"
	Completed  State = "completed"
	Unknown    State = "unknown"
)

var (
	ErrConflict = errors.New("idempotency key was reused with a different request")
	ErrNotFound = errors.New("idempotency record not found")
	ErrTerminal = errors.New("idempotency record is terminal")
)

var bucketRecords = []byte("idempotency_records_v1")

type Record struct {
	ProjectID   string    `json:"project_id"`
	KeyHash     string    `json:"key_hash"`
	Fingerprint string    `json:"fingerprint"`
	State       State     `json:"state"`
	RequestID   string    `json:"request_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type Store struct {
	db  *bbolt.DB
	now func() time.Time
}

func Open(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	if err := db.Update(func(tx *bbolt.Tx) error { _, e := tx.CreateBucketIfNotExists(bucketRecords); return e }); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
}
func (s *Store) Close() error { return s.db.Close() }

func ValidateKey(key string) error {
	if len(key) < 1 || len(key) > 128 {
		return errors.New("idempotency key must contain 1 to 128 characters")
	}
	for _, r := range key {
		if r > unicode.MaxASCII || unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("idempotency key must be visible ASCII without whitespace")
		}
	}
	return nil
}

func Fingerprint(operation string, canonicalRequest []byte) string {
	h := sha256.New()
	h.Write([]byte(operation))
	h.Write([]byte{0})
	h.Write(canonicalRequest)
	return hex.EncodeToString(h.Sum(nil))
}

func storageKey(projectID, key string) ([]byte, string, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, "", errors.New("project id is required")
	}
	if err := ValidateKey(key); err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256([]byte(key))
	hashed := hex.EncodeToString(digest[:])
	return []byte(projectID + "\x00" + hashed), hashed, nil
}

func (s *Store) Reserve(ctx context.Context, projectID, key, fingerprint, requestID string, ttl time.Duration) (Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, false, err
	}
	if len(fingerprint) != 64 {
		return Record{}, false, errors.New("SHA-256 fingerprint is required")
	}
	if ttl <= 0 {
		return Record{}, false, errors.New("positive retention is required")
	}
	storage, hashed, err := storageKey(projectID, key)
	if err != nil {
		return Record{}, false, err
	}
	now := s.now().UTC()
	created := false
	var out Record
	err = s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketRecords)
		raw := bucket.Get(storage)
		if raw != nil {
			if err := json.Unmarshal(raw, &out); err != nil {
				return fmt.Errorf("decode idempotency record: %w", err)
			}
			if now.Before(out.ExpiresAt) {
				if out.Fingerprint != fingerprint {
					return ErrConflict
				}
				return nil
			}
		}
		out = Record{ProjectID: projectID, KeyHash: hashed, Fingerprint: fingerprint, State: Reserved, RequestID: requestID, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(ttl)}
		encoded, e := json.Marshal(out)
		if e != nil {
			return e
		}
		created = true
		return bucket.Put(storage, encoded)
	})
	return out, created, err
}

func (s *Store) Transition(ctx context.Context, projectID, key string, next State) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	storage, _, err := storageKey(projectID, key)
	if err != nil {
		return Record{}, err
	}
	var out Record
	err = s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketRecords)
		raw := bucket.Get(storage)
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return err
		}
		if out.State == Completed || out.State == Unknown {
			return ErrTerminal
		}
		valid := (out.State == Reserved && next == InProgress) || (out.State == Reserved && (next == Completed || next == Unknown)) || (out.State == InProgress && (next == Completed || next == Unknown))
		if !valid {
			return fmt.Errorf("invalid idempotency transition %s to %s", out.State, next)
		}
		out.State = next
		out.UpdatedAt = s.now().UTC()
		encoded, e := json.Marshal(out)
		if e != nil {
			return e
		}
		return bucket.Put(storage, encoded)
	})
	return out, err
}

func (s *Store) Get(ctx context.Context, projectID, key string) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	storage, _, err := storageKey(projectID, key)
	if err != nil {
		return Record{}, err
	}
	var out Record
	err = s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketRecords).Get(storage)
		if raw == nil {
			return ErrNotFound
		}
		return json.Unmarshal(raw, &out)
	})
	return out, err
}
