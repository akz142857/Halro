package governance

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sort"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

// These bounds derive from the frozen WorkUnit declaration (8 definitions,
// 20 revisions each) and the native payload ceiling. They are not a calibrated
// physical memory budget or permission to truncate a malformed source.
const WorkUnitOutcomeMaxRows = domain.MaxDefinitionsPerWorkUnit * domain.MaxOutcomeRevisions
const WorkUnitOutcomeMaxBytes = WorkUnitOutcomeMaxRows * maxPayloadSize
const workUnitOutcomeMaxSourceBytes = WorkUnitOutcomeMaxRows*governanceProjectionMaxRow + domain.MaxDefinitionsPerWorkUnit*(domain.MaxGovernanceIDLength+2)

var ErrWorkUnitOutcomeLimit = errors.New("work unit outcome history exceeds its domain or byte boundary")

type WorkUnitOutcomeQuery struct {
	ProjectID, WorkUnitID string
	Definitions           []domain.OutcomeDefinitionRef
}

func validateWorkUnitOutcomeQuery(q WorkUnitOutcomeQuery) (map[string]uint64, error) {
	if q.ProjectID == "" || len(q.ProjectID) > maxPayloadSize || !domain.ValidWorkUnitID(q.WorkUnitID) || len(q.Definitions) > domain.MaxDefinitionsPerWorkUnit {
		return nil, ErrWorkUnitOutcomeLimit
	}
	definitions := make(map[string]uint64, len(q.Definitions))
	for _, ref := range q.Definitions {
		if !domain.ValidOutcomeDefinitionID(ref.ID) || ref.Version == 0 {
			return nil, ErrCorrupt
		}
		if _, exists := definitions[ref.ID]; exists {
			return nil, ErrCorrupt
		}
		definitions[ref.ID] = ref.Version
	}
	return definitions, nil
}

func appendWorkUnitOutcome(result *OutcomeSnapshot, row domain.Outcome, q WorkUnitOutcomeQuery, definitions map[string]uint64, size *int) error {
	if row.WorkUnitID != q.WorkUnitID || row.GovernanceSequence == 0 || row.GovernanceSequence > result.Sequence || row.Validate() != nil || row.Revision > domain.MaxOutcomeRevisions {
		return ErrCorrupt
	}
	if row.ProjectID != q.ProjectID {
		return nil
	}
	if definitions[row.DefinitionID] != row.DefinitionVersion {
		return ErrCorrupt
	}
	fields := len(row.ID) + len(row.ProjectID) + len(row.WorkUnitID) + len(row.DefinitionID) + len(row.Value) + len(row.ReporterKeyID) + len(row.EvidenceSHA256) + len(row.EvidenceRef) + len(row.SupersedesOutcomeID)
	if fields > maxPayloadSize {
		return ErrWorkUnitOutcomeLimit
	}
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if len(raw) > maxPayloadSize || len(raw) > WorkUnitOutcomeMaxBytes-*size {
		return ErrWorkUnitOutcomeLimit
	}
	*size += len(raw)
	result.Outcomes = append(result.Outcomes, row)
	return nil
}

func finishWorkUnitOutcomes(ctx context.Context, result OutcomeSnapshot, definitions map[string]uint64, headID func(string, uint64) (string, error)) (OutcomeSnapshot, error) {
	sort.Slice(result.Outcomes, func(i, j int) bool {
		return result.Outcomes[i].GovernanceSequence < result.Outcomes[j].GovernanceSequence
	})
	// A complete scoped history must contain contiguous revisions for every
	// declared definition/version. Check the chain after restoring native order;
	// an index gap or duplicate must fail, never become a shortened detail.
	current := make(map[string]domain.Outcome, len(definitions))
	var previousSequence uint64
	for _, row := range result.Outcomes {
		if err := ctx.Err(); err != nil {
			return OutcomeSnapshot{}, err
		}
		if row.GovernanceSequence <= previousSequence {
			return OutcomeSnapshot{}, ErrCorrupt
		}
		previousSequence = row.GovernanceSequence
		previous, found := current[row.DefinitionID]
		if !found && (row.Revision != 1 || row.SupersedesOutcomeID != "") || found && (row.Revision != previous.Revision+1 || row.SupersedesOutcomeID != previous.ID) {
			return OutcomeSnapshot{}, ErrCorrupt
		}
		current[row.DefinitionID] = row
	}
	for definition, version := range definitions {
		if err := ctx.Err(); err != nil {
			return OutcomeSnapshot{}, err
		}
		head, err := headID(definition, version)
		if err != nil {
			return OutcomeSnapshot{}, err
		}
		if head != current[definition].ID {
			return OutcomeSnapshot{}, ErrCorrupt
		}
	}
	if err := ctx.Err(); err != nil {
		return OutcomeSnapshot{}, err
	}
	return result, nil
}

// ReadWorkUnitOutcomes returns ALL revisions at one live native cut in native
// sequence order, scoped by the accounting owner's immutable declarations.
// It holds the Manager operation through one finite State lock/disk View. The
// result is not an atomic snapshot with the earlier independent Ledger read.
// All errors clear the result and are availability failures for detail callers.
func (m *Manager) ReadWorkUnitOutcomes(ctx context.Context, q WorkUnitOutcomeQuery) (OutcomeSnapshot, error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return OutcomeSnapshot{}, errors.Join(ErrUnavailable, err)
	}
	defer release()
	definitions, err := validateWorkUnitOutcomeQuery(q)
	if err != nil {
		return OutcomeSnapshot{}, errors.Join(ErrUnavailable, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	head, err := m.ownerHead(ctx)
	if err != nil {
		return OutcomeSnapshot{}, errors.Join(ErrUnavailable, err)
	}
	var result OutcomeSnapshot
	if m.projection != nil {
		result, err = m.projection.readWorkUnitOutcomes(ctx, q, definitions, head)
	} else if m.state != nil {
		result, err = m.state.readWorkUnitOutcomes(ctx, q, definitions, head)
	} else {
		err = ErrUnavailable
	}
	if err != nil {
		return OutcomeSnapshot{}, errors.Join(ErrUnavailable, err)
	}
	return result, nil
}

func (s *State) readWorkUnitOutcomes(ctx context.Context, q WorkUnitOutcomeQuery, definitions map[string]uint64, head Summary) (OutcomeSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.sequence != head.Records {
		return OutcomeSnapshot{}, ErrUnavailable
	}
	ids := s.byWorkUnit[q.WorkUnitID]
	if len(ids) > WorkUnitOutcomeMaxRows {
		return OutcomeSnapshot{}, ErrWorkUnitOutcomeLimit
	}
	result := OutcomeSnapshot{Outcomes: make([]domain.Outcome, 0, len(ids)), Sequence: head.Records, Offset: head.Bytes}
	size := 0
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return OutcomeSnapshot{}, err
		}
		row, found := s.outcomes[id]
		if !found || row.ID != id {
			return OutcomeSnapshot{}, ErrCorrupt
		}
		if err := appendWorkUnitOutcome(&result, row, q, definitions, &size); err != nil {
			return OutcomeSnapshot{}, err
		}
	}
	return finishWorkUnitOutcomes(ctx, result, definitions, func(definition string, version uint64) (string, error) {
		return s.heads[headKey(q.ProjectID, q.WorkUnitID, definition, version)], nil
	})
}

func (p *GovernanceProjection) readWorkUnitOutcomes(ctx context.Context, q WorkUnitOutcomeQuery, definitions map[string]uint64, cut Summary) (OutcomeSnapshot, error) {
	result := OutcomeSnapshot{Outcomes: make([]domain.Outcome, 0), Sequence: cut.Records, Offset: cut.Bytes}
	err := p.read(ctx, func(tx *bbolt.Tx, head Summary) error {
		if head != cut {
			return ErrUnavailable
		}
		prefix := govKey(q.WorkUnitID)
		c := tx.Bucket([]byte("by_work_unit")).Cursor()
		rows, sourceBytes, size := 0, 0, 0
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			if rows == WorkUnitOutcomeMaxRows {
				return ErrWorkUnitOutcomeLimit
			}
			rows++
			id := string(k[len(prefix):])
			if !domain.ValidOutcomeID(id) || len(v) != 8 {
				return ErrCorrupt
			}
			raw := tx.Bucket([]byte("outcomes")).Get([]byte(id))
			if len(raw) == 0 || len(raw) > governanceProjectionMaxRow {
				return ErrCorrupt
			}
			if len(raw) > workUnitOutcomeMaxSourceBytes-sourceBytes {
				return ErrWorkUnitOutcomeLimit
			}
			sourceBytes += len(raw)
			r, found, err := govRecord(tx, id, head)
			if err != nil || !found {
				return errors.Join(ErrCorrupt, err)
			}
			if r.Sequence != binary.BigEndian.Uint64(v) || r.Event.WorkUnitID != q.WorkUnitID {
				return ErrCorrupt
			}
			if err := appendWorkUnitOutcome(&result, recordOutcome(r), q, definitions, &size); err != nil {
				return err
			}
		}
		finished, err := finishWorkUnitOutcomes(ctx, result, definitions, func(definition string, version uint64) (string, error) {
			key := govHeadKey(q.ProjectID, q.WorkUnitID, definition, version)
			raw := tx.Bucket([]byte("heads")).Get(key)
			if len(raw) > domain.MaxGovernanceIDLength+2 {
				return "", ErrCorrupt
			}
			if len(raw) > workUnitOutcomeMaxSourceBytes-sourceBytes {
				return "", ErrWorkUnitOutcomeLimit
			}
			sourceBytes += len(raw)
			id, found, err := govGet[string](tx, "heads", key)
			if err != nil {
				return "", err
			}
			if found && !domain.ValidOutcomeID(id) {
				return "", ErrCorrupt
			}
			return id, nil
		})
		if err != nil {
			return err
		}
		result = finished
		return nil
	})
	if err != nil {
		return OutcomeSnapshot{}, err
	}
	return result, nil
}
