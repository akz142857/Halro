package governance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

const CurrentOutcomeBatchMaxRows = 64
const CurrentOutcomeBatchMaxBytes = 4 << 20

var ErrOutcomeFence = errors.New("outcome fact fence belongs to another owner or prefix")

// OutcomeFence is an ephemeral authority for one Manager's immutable fact
// prefix. It retains no Manager/State pointer and is not persisted cache trust.
type OutcomeFence struct {
	head           Summary
	authentication [32]byte
}

func (f OutcomeFence) Head() Summary { return f.head }

type CurrentOutcomeReference struct {
	ProjectID, WorkUnitID, DefinitionID string
	DefinitionVersion                   uint64
}

// Only minimal summary inputs cross the owner boundary, never evidence/history.
type CurrentOutcomeObservation struct {
	Reference CurrentOutcomeReference
	Found     bool
	Value     string
}

func outcomeFenceMAC(key []byte, head Summary) [32]byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte("halro:outcome-fence:v1"))
	h.Write(binary.BigEndian.AppendUint64(nil, head.Records))
	h.Write(binary.BigEndian.AppendUint64(nil, uint64(head.Bytes)))
	h.Write(head.LastHash[:])
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

func (m *Manager) CaptureOutcomeFence(ctx context.Context) (OutcomeFence, error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return OutcomeFence{}, err
	}
	defer release()
	m.mu.Lock()
	defer m.mu.Unlock()
	head, err := m.ownerHead(ctx)
	if err != nil {
		return OutcomeFence{}, err
	}
	if err := m.ensurePageKey(); err != nil {
		return OutcomeFence{}, err
	}
	return OutcomeFence{head: head, authentication: outcomeFenceMAC(m.pageKey, head)}, nil
}

func validateCurrentReferences(refs []CurrentOutcomeReference) error {
	if len(refs) > CurrentOutcomeBatchMaxRows {
		return ErrOutcomePageLimit
	}
	size := 0
	for _, ref := range refs {
		if ref.ProjectID == "" || len(ref.ProjectID) > maxPayloadSize || !domain.ValidWorkUnitID(ref.WorkUnitID) || !domain.ValidOutcomeDefinitionID(ref.DefinitionID) || ref.DefinitionVersion == 0 {
			return ErrOutcomePageLimit
		}
		size += len(ref.ProjectID) + len(ref.WorkUnitID) + len(ref.DefinitionID) + 8
		if size > OutcomePageMaxBytes {
			return ErrOutcomePageLimit
		}
	}
	return nil
}

func sameOutcomeReference(row domain.Outcome, ref CurrentOutcomeReference) bool {
	return row.ProjectID == ref.ProjectID && row.WorkUnitID == ref.WorkUnitID && row.DefinitionID == ref.DefinitionID && row.DefinitionVersion == ref.DefinitionVersion
}

// ReadCurrentOutcomeBatch returns the latest revision at the captured fence in
// reference order. Ordinary writes may continue between batches. One short
// lock/View, max 64 refs/4MiB source, at most 20 revision hops per unique ref.
// Limits/errors discard ALL observations. A caller may split a too-wide batch
// using the same fence; a single legal native revision chain fits this bound.
func (m *Manager) ReadCurrentOutcomeBatch(ctx context.Context, fence OutcomeFence, refs []CurrentOutcomeReference) ([]CurrentOutcomeObservation, error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateCurrentReferences(refs); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, err := m.ownerHead(ctx)
	if err != nil {
		return nil, err
	}
	expected := outcomeFenceMAC(m.pageKey, fence.head)
	if len(m.pageKey) == 0 || !hmac.Equal(expected[:], fence.authentication[:]) || fence.head.Bytes < 0 || fence.head.Records > current.Records || fence.head.Bytes > current.Bytes || fence.head.Records == current.Records && fence.head != current {
		return nil, ErrOutcomeFence
	}
	if m.projection != nil {
		return m.projection.readCurrentBatch(ctx, fence.head, refs)
	}
	if m.state == nil {
		return nil, ErrUnavailable
	}
	return m.state.readCurrentBatch(ctx, fence.head, refs)
}

func (s *State) readCurrentBatch(ctx context.Context, cut Summary, refs []CurrentOutcomeReference) ([]CurrentOutcomeObservation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if cut.Records > s.sequence {
		return nil, ErrUnavailable
	}
	result := make([]CurrentOutcomeObservation, 0, len(refs))
	cache := make(map[CurrentOutcomeReference]CurrentOutcomeObservation, len(refs))
	sourceBytes := 0
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if v, ok := cache[ref]; ok {
			result = append(result, v)
			continue
		}
		v := CurrentOutcomeObservation{Reference: ref}
		if cut.Records != 0 {
			key := headKey(ref.ProjectID, ref.WorkUnitID, ref.DefinitionID, ref.DefinitionVersion)
			history := s.history[key]
			if len(history) > domain.MaxOutcomeRevisions || len(history) == 0 && s.heads[key] != "" || len(history) > 0 && s.heads[key] != history[len(history)-1] {
				return nil, ErrCorrupt
			}
			for i := len(history) - 1; i >= 0; i-- {
				row, ok := s.outcomes[history[i]]
				if !ok || row.ID != history[i] || !sameOutcomeReference(row, ref) || row.Revision != uint64(i+1) || row.GovernanceSequence == 0 || row.GovernanceSequence > s.sequence || row.Validate() != nil {
					return nil, ErrCorrupt
				}
				if i == 0 && row.SupersedesOutcomeID != "" || i > 0 && (row.SupersedesOutcomeID != history[i-1] || s.outcomes[history[i-1]].GovernanceSequence >= row.GovernanceSequence) {
					return nil, ErrCorrupt
				}
				// State rows already exist; guard field sizes before encoding the
				// finite source-meter copy, then bound its encoded bytes as well.
				fields := len(row.ProjectID) + len(row.ReporterKeyID) + len(row.WorkUnitID) + len(row.DefinitionID) + len(row.ID) + len(row.Value) + len(row.EvidenceSHA256) + len(row.EvidenceRef) + len(row.SupersedesOutcomeID)
				if fields > governanceProjectionMaxRow {
					return nil, ErrOutcomePageLimit
				}
				raw, err := json.Marshal(row)
				if err != nil {
					return nil, err
				}
				if len(raw) > governanceProjectionMaxRow || len(raw) > CurrentOutcomeBatchMaxBytes-sourceBytes {
					return nil, ErrOutcomePageLimit
				}
				sourceBytes += len(raw)
				if row.GovernanceSequence <= cut.Records {
					v.Found, v.Value = true, row.Value
					break
				}
			}
		}
		cache[ref] = v
		result = append(result, v)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (p *GovernanceProjection) readCurrentBatch(ctx context.Context, cut Summary, refs []CurrentOutcomeReference) ([]CurrentOutcomeObservation, error) {
	result := make([]CurrentOutcomeObservation, 0, len(refs))
	err := p.read(ctx, func(tx *bbolt.Tx, head Summary) error {
		if cut.Records > head.Records || cut.Bytes > head.Bytes || cut.Records == head.Records && cut != head {
			return ErrOutcomeFence
		}
		cache := make(map[CurrentOutcomeReference]CurrentOutcomeObservation, len(refs))
		sourceBytes := 0
		for _, ref := range refs {
			if err := ctx.Err(); err != nil {
				return err
			}
			if v, ok := cache[ref]; ok {
				result = append(result, v)
				continue
			}
			v := CurrentOutcomeObservation{Reference: ref}
			if cut.Records != 0 {
				id, found, err := govGet[string](tx, "heads", govHeadKey(ref.ProjectID, ref.WorkUnitID, ref.DefinitionID, ref.DefinitionVersion))
				if err != nil {
					return err
				}
				var previous domain.Outcome
				for hop := 0; found; hop++ {
					if err := ctx.Err(); err != nil {
						return err
					}
					if hop >= domain.MaxOutcomeRevisions || !domain.ValidOutcomeID(id) {
						return ErrCorrupt
					}
					raw := tx.Bucket([]byte("outcomes")).Get([]byte(id))
					if len(raw) == 0 || len(raw) > governanceProjectionMaxRow {
						return ErrCorrupt
					}
					if len(raw) > CurrentOutcomeBatchMaxBytes-sourceBytes {
						return ErrOutcomePageLimit
					}
					sourceBytes += len(raw)
					r, ok, err := govRecord(tx, id, head)
					if err != nil || !ok {
						return errors.Join(ErrCorrupt, err)
					}
					row := recordOutcome(r)
					// Native sequence and physical offset must describe the same
					// side of this immutable fact cut; neither alone grants it.
					if (r.Sequence <= cut.Records) != (r.Offset <= cut.Bytes) {
						return ErrCorrupt
					}
					if !sameOutcomeReference(row, ref) || hop > 0 && (row.Revision+1 != previous.Revision || row.ID != previous.SupersedesOutcomeID || row.GovernanceSequence >= previous.GovernanceSequence) {
						return ErrCorrupt
					}
					if row.Revision == 1 && row.SupersedesOutcomeID != "" || row.Revision > 1 && row.SupersedesOutcomeID == "" {
						return ErrCorrupt
					}
					if row.GovernanceSequence <= cut.Records {
						v.Found, v.Value = true, row.Value
						break
					}
					if row.Revision == 1 {
						break
					}
					previous, id = row, row.SupersedesOutcomeID
				}
			}
			cache[ref] = v
			result = append(result, v)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
