package governance

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/ledger"
)

var (
	ErrUnavailable      = errors.New("governance journal is unavailable")
	ErrNotFound         = errors.New("governance resource was not found")
	ErrDefinitionDenied = errors.New("outcome definition was not declared by work unit")
	ErrRevisionConflict = errors.New("outcome revision conflict")
	ErrRevisionLimit    = errors.New("outcome revision limit exceeded")
	ErrWriteWindow      = errors.New("outcome write window has elapsed")
	ErrIdempotency      = errors.New("idempotency key conflicts with another request")
)

type DefinitionReader interface {
	GetOutcomeDefinition(context.Context, string, string, uint64) (domain.OutcomeDefinition, error)
}

type CheckpointStore interface {
	PutGovernanceJournalAnchor(sequence uint64, offset int64, hash, authentication [32]byte) error
}

type Intent struct {
	IdempotencyKeyHash string
	RequestFingerprint string
}

type ReportInput struct {
	ProjectID           string
	WorkUnitID          string
	DefinitionID        string
	Value               string
	ReporterKeyID       string
	EvidenceSHA256      string
	EvidenceRef         string
	ObservedAt          time.Time
	SupersedesOutcomeID string
	Intent              Intent
}

type idemRecord struct {
	Fingerprint string `json:"fingerprint"`
	OutcomeID   string `json:"outcome_id"`
}

type Snapshot struct {
	Version  int              `json:"version"`
	Sequence uint64           `json:"sequence"`
	Records  []SnapshotRecord `json:"records"`
}

type SnapshotRecord struct {
	Outcome            domain.Outcome `json:"outcome"`
	IdempotencyKeyHash string         `json:"idempotency_key_hash"`
	RequestFingerprint string         `json:"request_fingerprint"`
}

const SnapshotVersion = 1

type State struct {
	mu           sync.RWMutex
	outcomes     map[string]domain.Outcome
	outcomeOrder []string
	byProject    map[string][]string
	byWorkUnit   map[string][]string
	byDefinition map[string][]string
	heads        map[string]string
	history      map[string][]string
	idempotency  map[string]idemRecord
	intents      map[string]SnapshotRecord
	sequence     uint64
}

func NewState() *State {
	return &State{
		outcomes: map[string]domain.Outcome{}, byProject: map[string][]string{},
		byWorkUnit: map[string][]string{}, byDefinition: map[string][]string{},
		heads: map[string]string{}, history: map[string][]string{},
		idempotency: map[string]idemRecord{}, intents: map[string]SnapshotRecord{},
	}
}

func headKey(projectID, workUnitID, definitionID string, version uint64) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%d", projectID, workUnitID, definitionID, version)
}
func idemKey(projectID, hash string) string { return projectID + "\x00outcome.report\x00" + hash }

func (s *State) Apply(record Record) error {
	e := record.Event
	if err := e.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if record.Sequence != s.sequence+1 {
		return errors.New("governance sequence is not contiguous")
	}
	if _, exists := s.outcomes[e.OutcomeID]; exists {
		return errors.New("outcome id already exists")
	}
	key := headKey(e.ProjectID, e.WorkUnitID, e.DefinitionID, e.DefinitionVersion)
	history := s.history[key]
	if int(e.Revision) != len(history)+1 {
		return errors.New("outcome revision is not contiguous")
	}
	if len(history) == 0 {
		if e.SupersedesOutcomeID != "" {
			return ErrRevisionConflict
		}
	} else if s.heads[key] != e.SupersedesOutcomeID {
		return ErrRevisionConflict
	}
	if len(history) >= domain.MaxOutcomeRevisions {
		return ErrRevisionLimit
	}
	outcome := domain.Outcome{
		ID: e.OutcomeID, ProjectID: e.ProjectID, WorkUnitID: e.WorkUnitID,
		DefinitionID: e.DefinitionID, DefinitionVersion: e.DefinitionVersion,
		Value: e.Value, ReporterKeyID: e.ReporterKeyID, EvidenceSHA256: e.EvidenceSHA256,
		EvidenceRef: e.EvidenceRef, ObservedAt: e.ObservedAt, IngestedAt: e.IngestedAt,
		SupersedesOutcomeID: e.SupersedesOutcomeID, Revision: e.Revision, GovernanceSequence: record.Sequence,
	}
	s.outcomes[outcome.ID] = outcome
	s.outcomeOrder = append(s.outcomeOrder, outcome.ID)
	s.byProject[outcome.ProjectID] = append(s.byProject[outcome.ProjectID], outcome.ID)
	s.byWorkUnit[outcome.WorkUnitID] = append(s.byWorkUnit[outcome.WorkUnitID], outcome.ID)
	s.byDefinition[outcome.DefinitionID] = append(s.byDefinition[outcome.DefinitionID], outcome.ID)
	s.history[key] = append(history, outcome.ID)
	s.heads[key] = outcome.ID
	s.idempotency[idemKey(e.ProjectID, e.IdempotencyKeyHash)] = idemRecord{Fingerprint: e.RequestFingerprint, OutcomeID: outcome.ID}
	s.intents[outcome.ID] = SnapshotRecord{Outcome: outcome, IdempotencyKeyHash: e.IdempotencyKeyHash, RequestFingerprint: e.RequestFingerprint}
	s.sequence = record.Sequence
	return nil
}

func (s *State) Outcome(id string) (domain.Outcome, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.outcomes[id]
	return value, ok
}

func (s *State) Current(projectID, workUnitID, definitionID string, version uint64) (domain.Outcome, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id := s.heads[headKey(projectID, workUnitID, definitionID, version)]
	value, ok := s.outcomes[id]
	return value, ok
}

func (s *State) Outcomes(projectID, workUnitID, definitionID string) []domain.Outcome {
	items, _ := s.OutcomesContext(context.Background(), projectID, workUnitID, definitionID)
	return items
}

func (s *State) OutcomesContext(ctx context.Context, projectID, workUnitID, definitionID string) ([]domain.Outcome, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	candidates := s.outcomeOrder
	filters := []struct {
		value string
		index map[string][]string
	}{{projectID, s.byProject}, {workUnitID, s.byWorkUnit}, {definitionID, s.byDefinition}}
	for _, filter := range filters {
		if filter.value == "" {
			continue
		}
		indexed, ok := filter.index[filter.value]
		if !ok {
			return []domain.Outcome{}, nil
		}
		if len(indexed) < len(candidates) {
			candidates = indexed
		}
	}
	items := make([]domain.Outcome, 0, len(candidates))
	for _, outcomeID := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item := s.outcomes[outcomeID]
		if (projectID == "" || item.ProjectID == projectID) && (workUnitID == "" || item.WorkUnitID == workUnitID) && (definitionID == "" || item.DefinitionID == definitionID) {
			items = append(items, item)
		}
	}
	return items, nil
}

func (s *State) Idempotency(projectID, hash string) (idemRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.idempotency[idemKey(projectID, hash)]
	return value, ok
}

func (s *State) Sequence() uint64 { s.mu.RLock(); defer s.mu.RUnlock(); return s.sequence }

func (s *State) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	records := make([]SnapshotRecord, 0, len(s.outcomes))
	for id := range s.outcomes {
		records = append(records, s.intents[id])
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].Outcome.GovernanceSequence < records[j].Outcome.GovernanceSequence
	})
	return Snapshot{Version: SnapshotVersion, Sequence: s.sequence, Records: records}
}

func (s *State) Restore(snapshot Snapshot) error {
	if snapshot.Version != SnapshotVersion {
		return errors.New("governance checkpoint version is unsupported")
	}
	rebuilt := NewState()
	for _, checkpoint := range snapshot.Records {
		outcome := checkpoint.Outcome
		event := Event{EventID: "checkpoint:" + outcome.ID, ProjectID: outcome.ProjectID, WorkUnitID: outcome.WorkUnitID,
			DefinitionID: outcome.DefinitionID, DefinitionVersion: outcome.DefinitionVersion, OutcomeID: outcome.ID,
			Value: outcome.Value, ReporterKeyID: outcome.ReporterKeyID, EvidenceSHA256: outcome.EvidenceSHA256,
			EvidenceRef: outcome.EvidenceRef, ObservedAt: outcome.ObservedAt, IngestedAt: outcome.IngestedAt,
			SupersedesOutcomeID: outcome.SupersedesOutcomeID, Revision: outcome.Revision,
			IdempotencyKeyHash: checkpoint.IdempotencyKeyHash, RequestFingerprint: checkpoint.RequestFingerprint}
		if err := rebuilt.Apply(Record{Sequence: outcome.GovernanceSequence, Event: event}); err != nil {
			return err
		}
	}
	if rebuilt.sequence != snapshot.Sequence {
		return errors.New("governance checkpoint sequence does not match outcomes")
	}
	s.mu.Lock()
	s.outcomes, s.outcomeOrder = rebuilt.outcomes, rebuilt.outcomeOrder
	s.byProject, s.byWorkUnit, s.byDefinition = rebuilt.byProject, rebuilt.byWorkUnit, rebuilt.byDefinition
	s.heads, s.history, s.idempotency, s.intents, s.sequence = rebuilt.heads, rebuilt.history, rebuilt.idempotency, rebuilt.intents, rebuilt.sequence
	s.mu.Unlock()
	return nil
}

type Manager struct {
	mu          sync.Mutex
	log         *Log
	state       *State
	accounting  *ledger.State
	definitions DefinitionReader
	checkpoint  CheckpointStore
	now         func() time.Time
	unavailable error
}

func NewManager(log *Log, state *State, accounting *ledger.State, definitions DefinitionReader, checkpoint CheckpointStore) *Manager {
	return &Manager{log: log, state: state, accounting: accounting, definitions: definitions, checkpoint: checkpoint, now: time.Now}
}

func NewUnavailable(err error) *Manager {
	return &Manager{state: NewState(), unavailable: errors.Join(ErrUnavailable, err), now: time.Now}
}

func (m *Manager) Ready() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.unavailable == nil, m.unavailable
}
func (m *Manager) State() *State { return m.state }

func (m *Manager) Report(ctx context.Context, input ReportInput) (domain.Outcome, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unavailable != nil || m.log == nil || m.accounting == nil || m.definitions == nil {
		return domain.Outcome{}, false, ErrUnavailable
	}
	if previous, ok := m.state.Idempotency(input.ProjectID, input.Intent.IdempotencyKeyHash); ok {
		if previous.Fingerprint != input.Intent.RequestFingerprint {
			return domain.Outcome{}, false, ErrIdempotency
		}
		outcome, found := m.state.Outcome(previous.OutcomeID)
		if !found {
			return domain.Outcome{}, false, ErrUnavailable
		}
		if outcome.ProjectID != input.ProjectID || outcome.WorkUnitID != input.WorkUnitID || outcome.DefinitionID != input.DefinitionID {
			return domain.Outcome{}, false, ErrIdempotency
		}
		return outcome, true, nil
	}
	workUnit, ok := m.accounting.WorkUnit(input.WorkUnitID)
	if !ok || workUnit.ProjectID != input.ProjectID {
		return domain.Outcome{}, false, ErrNotFound
	}
	var reference domain.OutcomeDefinitionRef
	for _, item := range workUnit.OutcomeDefinitions {
		if item.ID == input.DefinitionID {
			reference = item
			break
		}
	}
	if reference.ID == "" {
		return domain.Outcome{}, false, ErrDefinitionDenied
	}
	definition, err := m.definitions.GetOutcomeDefinition(ctx, input.ProjectID, reference.ID, reference.Version)
	if err != nil {
		return domain.Outcome{}, false, ErrNotFound
	}
	if !definition.Allows(input.Value) {
		return domain.Outcome{}, false, errors.New("outcome value is outside the definition")
	}
	if input.ObservedAt.IsZero() {
		return domain.Outcome{}, false, errors.New("observed_at is required")
	}
	if err := domain.ValidateEvidenceRef(input.EvidenceRef); err != nil {
		return domain.Outcome{}, false, err
	}
	if input.EvidenceSHA256 != "" && !domain.ValidEvidenceSHA256(input.EvidenceSHA256) {
		return domain.Outcome{}, false, errors.New("evidence_sha256 must be 64 lowercase hex characters")
	}
	now := m.now().UTC()
	if workUnit.ClosedAt != nil && now.After(workUnit.ClosedAt.Add(domain.OutcomeWriteWindow)) {
		return domain.Outcome{}, false, ErrWriteWindow
	}
	current, hasCurrent := m.state.Current(input.ProjectID, input.WorkUnitID, reference.ID, reference.Version)
	revision := uint64(1)
	if hasCurrent {
		if input.SupersedesOutcomeID != current.ID {
			return domain.Outcome{}, false, ErrRevisionConflict
		}
		if current.Revision >= domain.MaxOutcomeRevisions {
			return domain.Outcome{}, false, ErrRevisionLimit
		}
		revision = current.Revision + 1
	} else if input.SupersedesOutcomeID != "" {
		return domain.Outcome{}, false, ErrRevisionConflict
	}
	outcomeID, err := id.New("out")
	if err != nil {
		return domain.Outcome{}, false, err
	}
	eventID, err := id.New("gov")
	if err != nil {
		return domain.Outcome{}, false, err
	}
	event := Event{EventID: eventID, ProjectID: input.ProjectID, WorkUnitID: input.WorkUnitID,
		DefinitionID: reference.ID, DefinitionVersion: reference.Version, OutcomeID: outcomeID,
		Value: input.Value, ReporterKeyID: input.ReporterKeyID, EvidenceSHA256: input.EvidenceSHA256,
		EvidenceRef: input.EvidenceRef, ObservedAt: input.ObservedAt, IngestedAt: now,
		SupersedesOutcomeID: input.SupersedesOutcomeID, Revision: revision,
		IdempotencyKeyHash: input.Intent.IdempotencyKeyHash, RequestFingerprint: input.Intent.RequestFingerprint}
	record, err := m.log.Append(ctx, event)
	if err != nil {
		m.unavailable = errors.Join(ErrUnavailable, err)
		return domain.Outcome{}, false, m.unavailable
	}
	if err := m.state.Apply(record); err != nil {
		m.unavailable = errors.Join(ErrUnavailable, err)
		return domain.Outcome{}, false, m.unavailable
	}
	if m.checkpoint != nil {
		summary, authentication := m.log.AuthenticatedHead()
		if err := m.checkpoint.PutGovernanceJournalAnchor(summary.Records, summary.Bytes, summary.LastHash, authentication); err != nil {
			m.unavailable = errors.Join(ErrUnavailable, err)
			return domain.Outcome{}, false, m.unavailable
		}
	}
	outcome, _ := m.state.Outcome(outcomeID)
	outcome.Provisional = workUnit.Status == domain.WorkUnitOpen || m.workUnitInflight(workUnit.ID)
	return outcome, false, nil
}

func (m *Manager) workUnitInflight(workUnitID string) bool {
	workUnit, ok := m.accounting.WorkUnit(workUnitID)
	if !ok {
		return true
	}
	snapshot, err := m.accounting.GovernanceSnapshot(context.Background(), ledger.GovernanceSnapshotOptions{
		ProjectID: workUnit.ProjectID, WorkUnitID: workUnitID,
	})
	return err != nil || snapshot.InflightWorkUnit[workUnitID]
}

func (m *Manager) Outcomes(projectID, workUnitID, definitionID string) []domain.Outcome {
	items := m.state.Outcomes(projectID, workUnitID, definitionID)
	for index := range items {
		wu, ok := m.accounting.WorkUnit(items[index].WorkUnitID)
		items[index].Provisional = !ok || wu.Status == domain.WorkUnitOpen || m.workUnitInflight(wu.ID)
	}
	return slices.Clone(items)
}

type OutcomeSnapshot struct {
	Outcomes []domain.Outcome
	Sequence uint64
	Offset   int64
}

// ReadOutcomeSnapshot returns Outcome rows and the exact journal watermark
// that produced them. It deliberately leaves accounting-derived provisional
// state to the caller's separate accounting snapshot.
func (m *Manager) ReadOutcomeSnapshot(ctx context.Context, projectID, workUnitID, definitionID string) (OutcomeSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unavailable != nil || m.log == nil {
		return OutcomeSnapshot{}, ErrUnavailable
	}
	items, err := m.state.OutcomesContext(ctx, projectID, workUnitID, definitionID)
	if err != nil {
		return OutcomeSnapshot{}, err
	}
	summary := m.log.Summary()
	return OutcomeSnapshot{
		Outcomes: items,
		Sequence: summary.Records,
		Offset:   summary.Bytes,
	}, nil
}
