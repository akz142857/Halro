// Package archive keeps what has left the in-memory accounting and governance
// state for good: finished Work Units with their Runs, cost totals and
// idempotency records, and Outcomes (docs/todo/memory-bounded-redesign.zh-CN.md
// §13).
//
// It is a separate file from the usage history (history.db) because the two
// have different owners and different lifetimes. The usage history is a
// derivative that may be emptied and rebuilt at any time; the state that wrote
// rows here has already forgotten them, so this file is emptied only together
// with the snapshot that relies on it — which a start detects through the
// recorded positions and answers by rebuilding both from the logs.
package archive

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

// formatVersion is bumped whenever a stored shape changes. A file written
// under another version is emptied; its recorded positions go with it, so
// every snapshot that relied on it is refused and rebuilt.
const formatVersion = 1

const macSize = sha256.Size

var (
	bucketMeta          = []byte("meta")
	bucketWorkUnits     = []byte("work_units")
	bucketWorkUnitIndex = []byte("work_unit_index")
	bucketRuns          = []byte("runs")
	bucketRunIndex      = []byte("run_index")
	bucketIdempotency   = []byte("idempotency")
	allBuckets          = [][]byte{bucketMeta, bucketWorkUnits, bucketWorkUnitIndex, bucketRuns, bucketRunIndex, bucketIdempotency}

	keyVersion       = []byte("version")
	keyCheck         = []byte("key_check")
	keyLedgerThrough = []byte("ledger_through")
	keyCheckLabel    = []byte("halro archive key check v1")

	// ErrTampered is a row that failed authentication. A read that meets
	// one fails; it never skips the row.
	ErrTampered = errors.New("archive row failed authentication")
)

// Store is archive.db.
type Store struct {
	db  *bbolt.DB
	key []byte
}

// Open opens or creates the archive. reset reports that the file it found was
// written under another format version or another key and was emptied.
func Open(path string, key []byte) (store *Store, reset bool, err error) {
	if len(key) != 32 {
		return nil, false, errors.New("archive key must be 32 bytes")
	}
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, false, fmt.Errorf("open archive: %w", err)
	}
	store = &Store{db: db, key: bytes.Clone(key)}
	check := store.mac(bucketMeta, keyCheck, keyCheckLabel)
	err = db.Update(func(tx *bbolt.Tx) error {
		if meta := tx.Bucket(bucketMeta); meta != nil {
			version := meta.Get(keyVersion)
			if len(version) == 8 && binary.BigEndian.Uint64(version) == formatVersion && hmac.Equal(meta.Get(keyCheck), check) {
				for _, name := range allBuckets {
					if tx.Bucket(name) == nil {
						return fmt.Errorf("archive bucket %s is missing", name)
					}
				}
				return nil
			}
			reset = true
		}
		for _, name := range allBuckets {
			if tx.Bucket(name) != nil {
				if err := tx.DeleteBucket(name); err != nil {
					return err
				}
			}
			if _, err := tx.CreateBucket(name); err != nil {
				return err
			}
		}
		meta := tx.Bucket(bucketMeta)
		if err := meta.Put(keyVersion, binary.BigEndian.AppendUint64(nil, formatVersion)); err != nil {
			return err
		}
		return meta.Put(keyCheck, check)
	})
	if err != nil {
		db.Close()
		return nil, false, fmt.Errorf("initialize archive: %w", err)
	}
	return store, reset, nil
}

func (s *Store) Close() error {
	clear(s.key)
	return s.db.Close()
}

// mac authenticates one value under its bucket and key, so a row can be
// neither edited nor moved to another key or bucket without failing.
func (s *Store) mac(bucket, key, value []byte) []byte {
	mac := hmac.New(sha256.New, s.key)
	var length [8]byte
	for _, part := range [][]byte{bucket, key, value} {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		mac.Write(length[:])
		mac.Write(part)
	}
	return mac.Sum(nil)
}

func (s *Store) put(bucket *bbolt.Bucket, name, key []byte, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return bucket.Put(key, append(encoded, s.mac(name, key, encoded)...))
}

func (s *Store) get(name, key, raw []byte, target any) error {
	if len(raw) < macSize {
		return ErrTampered
	}
	body, tag := raw[:len(raw)-macSize], raw[len(raw)-macSize:]
	if !hmac.Equal(tag, s.mac(name, key, body)) {
		return ErrTampered
	}
	return json.Unmarshal(body, target)
}

func joinKey(parts ...string) []byte {
	var key []byte
	for index, part := range parts {
		if index > 0 {
			key = append(key, 0)
		}
		key = append(key, part...)
	}
	return key
}

func idempotencyKey(projectID, operation, keyHash string) []byte {
	return joinKey(projectID, operation, keyHash)
}

// through reads one recorded position.
func (s *Store) through(name []byte) (uint64, error) {
	var value uint64
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(name)
		if raw == nil {
			return nil
		}
		return s.get(bucketMeta, name, raw, &value)
	})
	return value, err
}

// advance records a position, never moving it backwards.
func (s *Store) advance(tx *bbolt.Tx, name []byte, value uint64) error {
	meta := tx.Bucket(bucketMeta)
	if raw := meta.Get(name); raw != nil {
		var current uint64
		if err := s.get(bucketMeta, name, raw, &current); err != nil {
			return err
		}
		if value < current {
			return nil
		}
	}
	return s.put(meta, bucketMeta, name, value)
}

var _ ledger.Archive = (*Store)(nil)

func (s *Store) WriteArchived(items []ledger.ArchivedWorkUnit, through uint64) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		workUnits, index := tx.Bucket(bucketWorkUnits), tx.Bucket(bucketWorkUnitIndex)
		runs, runIndex, idempotency := tx.Bucket(bucketRuns), tx.Bucket(bucketRunIndex), tx.Bucket(bucketIdempotency)
		for _, item := range items {
			id := []byte(item.WorkUnit.ID)
			if err := s.put(workUnits, bucketWorkUnits, id, item); err != nil {
				return err
			}
			if err := index.Put(joinKey(item.WorkUnit.ProjectID, item.WorkUnit.ID), nil); err != nil {
				return err
			}
			for _, run := range item.Runs {
				if err := s.put(runs, bucketRuns, []byte(run.ID), run); err != nil {
					return err
				}
				if err := runIndex.Put(joinKey(run.ProjectID, run.ID), nil); err != nil {
					return err
				}
			}
			for _, record := range item.Idempotency {
				if err := s.put(idempotency, bucketIdempotency, idempotencyKey(record.ProjectID, record.Operation, record.KeyHash), record); err != nil {
					return err
				}
			}
		}
		return s.advance(tx, keyLedgerThrough, through)
	})
}

func (s *Store) ArchivedThrough() (uint64, error) { return s.through(keyLedgerThrough) }

func (s *Store) ArchivedWorkUnit(id string) (ledger.ArchivedWorkUnit, bool, error) {
	var item ledger.ArchivedWorkUnit
	found := false
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketWorkUnits).Get([]byte(id))
		if raw == nil {
			return nil
		}
		found = true
		if err := s.get(bucketWorkUnits, []byte(id), raw, &item); err != nil {
			return err
		}
		if item.WorkUnit.ID != id {
			return ErrTampered
		}
		return nil
	})
	return item, found && err == nil, err
}

func (s *Store) ArchivedRun(id string) (domain.Run, bool, error) {
	var run domain.Run
	found := false
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketRuns).Get([]byte(id))
		if raw == nil {
			return nil
		}
		found = true
		if err := s.get(bucketRuns, []byte(id), raw, &run); err != nil {
			return err
		}
		if run.ID != id {
			return ErrTampered
		}
		return nil
	})
	return run, found && err == nil, err
}

func (s *Store) ArchivedIdempotency(projectID, operation, keyHash string) (ledger.GovernanceIdempotency, bool, error) {
	var record ledger.GovernanceIdempotency
	found := false
	key := idempotencyKey(projectID, operation, keyHash)
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketIdempotency).Get(key)
		if raw == nil {
			return nil
		}
		found = true
		return s.get(bucketIdempotency, key, raw, &record)
	})
	return record, found && err == nil, err
}

// VisitArchivedWorkUnits walks Work Units in ID order: through the project
// index when a project is named, the rows themselves otherwise. An index
// entry carries no MAC, so the row it names must exist and agree with it.
func (s *Store) VisitArchivedWorkUnits(query ledger.ArchiveQuery, visit func(ledger.ArchivedWorkUnit) (bool, error)) error {
	return s.db.View(func(tx *bbolt.Tx) error {
		workUnits := tx.Bucket(bucketWorkUnits)
		emit := func(id []byte) (bool, error) {
			raw := workUnits.Get(id)
			if raw == nil {
				return false, ErrTampered
			}
			var item ledger.ArchivedWorkUnit
			if err := s.get(bucketWorkUnits, id, raw, &item); err != nil {
				return false, err
			}
			if item.WorkUnit.ID != string(id) || query.ProjectID != "" && item.WorkUnit.ProjectID != query.ProjectID {
				return false, ErrTampered
			}
			if query.CohortStart != "" && (item.WorkUnit.PeriodID < query.CohortStart || item.WorkUnit.PeriodID > query.CohortEnd) {
				return true, nil
			}
			return visit(item)
		}
		return visitOrdered(tx, bucketWorkUnits, bucketWorkUnitIndex, query.ProjectID, query.After, emit)
	})
}

// visitOrdered walks a bucket's rows in key order after a cursor, or a
// project's index entries in the index bucket when projectID is named, and
// hands each row's key to emit.
func visitOrdered(tx *bbolt.Tx, rows, index []byte, projectID, after string, emit func(id []byte) (bool, error)) error {
	if projectID == "" {
		cursor := tx.Bucket(rows).Cursor()
		key, _ := cursor.Seek([]byte(after))
		for ; key != nil; key, _ = cursor.Next() {
			if string(key) <= after {
				continue
			}
			if more, err := emit(key); err != nil || !more {
				return err
			}
		}
		return nil
	}
	prefix := joinKey(projectID, "")
	cursor := tx.Bucket(index).Cursor()
	key, _ := cursor.Seek(joinKey(projectID, after))
	for ; key != nil && bytes.HasPrefix(key, prefix); key, _ = cursor.Next() {
		id := key[len(prefix):]
		if string(id) <= after {
			continue
		}
		if more, err := emit(id); err != nil || !more {
			return err
		}
	}
	return nil
}

// VisitArchivedRuns walks Runs in ID order. An index entry carries no MAC, so
// the row it names must exist and agree with it.
func (s *Store) VisitArchivedRuns(projectID, after string, visit func(domain.Run) (bool, error)) error {
	return s.db.View(func(tx *bbolt.Tx) error {
		runs := tx.Bucket(bucketRuns)
		return visitOrdered(tx, bucketRuns, bucketRunIndex, projectID, after, func(id []byte) (bool, error) {
			raw := runs.Get(id)
			if raw == nil {
				return false, ErrTampered
			}
			var run domain.Run
			if err := s.get(bucketRuns, id, raw, &run); err != nil {
				return false, err
			}
			if run.ID != string(id) || projectID != "" && run.ProjectID != projectID {
				return false, ErrTampered
			}
			return visit(run)
		})
	})
}
