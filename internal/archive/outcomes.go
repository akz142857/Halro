package archive

import (
	"strconv"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
	bbolt "go.etcd.io/bbolt"
)

var (
	bucketOutcomes           = []byte("outcomes")
	bucketOutcomeProject     = []byte("outcome_project_index")
	bucketOutcomeWorkUnit    = []byte("outcome_work_unit_index")
	bucketOutcomeHeads       = []byte("outcome_heads")
	bucketOutcomeIdempotency = []byte("outcome_idempotency")
	outcomeBuckets           = [][]byte{bucketOutcomes, bucketOutcomeProject, bucketOutcomeWorkUnit, bucketOutcomeHeads, bucketOutcomeIdempotency}

	keyGovernanceThrough = []byte("governance_through")
	keyGovernanceHash    = []byte("governance_hash")
)

// outcomeHead is the current revision of one (Work Unit, definition,
// version).
type outcomeHead struct {
	OutcomeID string `json:"outcome_id"`
	Revision  uint64 `json:"revision"`
}

type outcomeIdempotency struct {
	Fingerprint string `json:"fingerprint"`
	OutcomeID   string `json:"outcome_id"`
}

func outcomeHeadKey(projectID, workUnitID, definitionID string, version uint64) []byte {
	return joinKey(projectID, workUnitID, definitionID, strconv.FormatUint(version, 10))
}

var _ governance.Archive = (*Store)(nil)

// WriteOutcomes stores items in journal order, so a later revision of the
// same head is the one that remains its head.
func (s *Store) WriteOutcomes(items []governance.SnapshotRecord, through uint64, hash [32]byte) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		outcomes, heads, idempotency := tx.Bucket(bucketOutcomes), tx.Bucket(bucketOutcomeHeads), tx.Bucket(bucketOutcomeIdempotency)
		byProject, byWorkUnit := tx.Bucket(bucketOutcomeProject), tx.Bucket(bucketOutcomeWorkUnit)
		for _, item := range items {
			outcome := item.Outcome
			id := []byte(outcome.ID)
			if err := s.put(outcomes, bucketOutcomes, id, item); err != nil {
				return err
			}
			if err := byProject.Put(joinKey(outcome.ProjectID, outcome.ID), nil); err != nil {
				return err
			}
			if err := byWorkUnit.Put(joinKey(outcome.WorkUnitID, outcome.ID), nil); err != nil {
				return err
			}
			head := outcomeHeadKey(outcome.ProjectID, outcome.WorkUnitID, outcome.DefinitionID, outcome.DefinitionVersion)
			if err := s.put(heads, bucketOutcomeHeads, head, outcomeHead{OutcomeID: outcome.ID, Revision: outcome.Revision}); err != nil {
				return err
			}
			key := joinKey(outcome.ProjectID, item.IdempotencyKeyHash)
			if err := s.put(idempotency, bucketOutcomeIdempotency, key, outcomeIdempotency{Fingerprint: item.RequestFingerprint, OutcomeID: outcome.ID}); err != nil {
				return err
			}
		}
		advanced, err := s.advance(tx, keyGovernanceThrough, through)
		if err != nil || !advanced {
			return err
		}
		return s.put(tx.Bucket(bucketMeta), bucketMeta, keyGovernanceHash, hash)
	})
}

func (s *Store) OutcomesThrough() (uint64, [32]byte, error) {
	through, err := s.through(keyGovernanceThrough)
	if err != nil || through == 0 {
		return 0, [32]byte{}, err
	}
	var hash [32]byte
	err = s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyGovernanceHash)
		if raw == nil {
			return ErrTampered
		}
		return s.get(bucketMeta, keyGovernanceHash, raw, &hash)
	})
	return through, hash, err
}

func (s *Store) ResetOutcomes() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := recreate(tx, outcomeBuckets); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		if err := meta.Delete(keyGovernanceThrough); err != nil {
			return err
		}
		return meta.Delete(keyGovernanceHash)
	})
}

// outcomeTx reads one Outcome row inside tx.
func (s *Store) outcomeTx(tx *bbolt.Tx, id []byte) (domain.Outcome, bool, error) {
	raw := tx.Bucket(bucketOutcomes).Get(id)
	if raw == nil {
		return domain.Outcome{}, false, nil
	}
	var item governance.SnapshotRecord
	if err := s.get(bucketOutcomes, id, raw, &item); err != nil {
		return domain.Outcome{}, false, err
	}
	if item.Outcome.ID != string(id) {
		return domain.Outcome{}, false, ErrTampered
	}
	return item.Outcome, true, nil
}

func (s *Store) ArchivedOutcome(id string) (domain.Outcome, bool, error) {
	var outcome domain.Outcome
	var found bool
	err := s.db.View(func(tx *bbolt.Tx) error {
		var err error
		outcome, found, err = s.outcomeTx(tx, []byte(id))
		return err
	})
	return outcome, found && err == nil, err
}

// ArchivedHead reads the head row and the Outcome it names, which must agree
// with it.
func (s *Store) ArchivedHead(projectID, workUnitID, definitionID string, version uint64) (domain.Outcome, bool, error) {
	var outcome domain.Outcome
	var found bool
	err := s.db.View(func(tx *bbolt.Tx) error {
		key := outcomeHeadKey(projectID, workUnitID, definitionID, version)
		raw := tx.Bucket(bucketOutcomeHeads).Get(key)
		if raw == nil {
			return nil
		}
		var head outcomeHead
		if err := s.get(bucketOutcomeHeads, key, raw, &head); err != nil {
			return err
		}
		var err error
		outcome, found, err = s.outcomeTx(tx, []byte(head.OutcomeID))
		if err != nil {
			return err
		}
		if !found || outcome.Revision != head.Revision || outcome.ProjectID != projectID || outcome.WorkUnitID != workUnitID ||
			outcome.DefinitionID != definitionID || outcome.DefinitionVersion != version {
			found = false
			return ErrTampered
		}
		return nil
	})
	return outcome, found && err == nil, err
}

func (s *Store) ArchivedOutcomeIdempotency(projectID, keyHash string) (string, string, bool, error) {
	var record outcomeIdempotency
	found := false
	key := joinKey(projectID, keyHash)
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketOutcomeIdempotency).Get(key)
		if raw == nil {
			return nil
		}
		found = true
		return s.get(bucketOutcomeIdempotency, key, raw, &record)
	})
	return record.Fingerprint, record.OutcomeID, found && err == nil, err
}

// VisitArchivedOutcomes walks Outcomes in ID order, through the Work Unit
// index when one is named, the project index when a project is, and the rows
// otherwise. An index entry carries no MAC, so the row it names must exist
// and agree with it.
func (s *Store) VisitArchivedOutcomes(query governance.OutcomeQuery, visit func(domain.Outcome) (bool, error)) error {
	return s.db.View(func(tx *bbolt.Tx) error {
		index, scope := bucketOutcomeProject, query.ProjectID
		if query.WorkUnitID != "" {
			index, scope = bucketOutcomeWorkUnit, query.WorkUnitID
		}
		return visitOrdered(tx, bucketOutcomes, index, scope, query.After, func(id []byte) (bool, error) {
			outcome, found, err := s.outcomeTx(tx, id)
			if err != nil {
				return false, err
			}
			if !found || query.WorkUnitID != "" && outcome.WorkUnitID != query.WorkUnitID ||
				query.WorkUnitID == "" && query.ProjectID != "" && outcome.ProjectID != query.ProjectID {
				return false, ErrTampered
			}
			if query.ProjectID != "" && outcome.ProjectID != query.ProjectID || query.DefinitionID != "" && outcome.DefinitionID != query.DefinitionID {
				return true, nil
			}
			return visit(outcome)
		})
	})
}
