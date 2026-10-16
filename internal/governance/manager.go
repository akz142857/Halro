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

// SnapshotRecord is one Outcome with the intent that wrote it, as the
// archive keeps it.
type SnapshotRecord struct {
	Outcome            domain.Outcome `json:"outcome"`
	IdempotencyKeyHash string         `json:"idempotency_key_hash"`
	RequestFingerprint string         `json:"request_fingerprint"`
}

// The state keeps no Outcome for good (docs/todo/memory-bounded-redesign.zh-CN.md
// §13.2). Each one waits in a queue until it is in the archive — written as
// soon as it is reported, and every outcomeFlushThreshold records during a
// replay. What the state keeps are two rings: the revision heads and the
// idempotency records most recently touched, recentOutcomes of each. The
// queue never holds more than outcomeFlushThreshold, fewer than the rings
// hold, so a queued Outcome's head and idempotency record are always in them.
//
// Apply checks a revision against its head when the ring has it, and takes it
// on trust when the head has left: the Manager checked it against the full
// head (ring, then archive) before appending, and the journal is chained under
// a MAC. Which heads the ring has depends only on the journal, so a replay
// checks exactly what the live instance checked.
const (
	recentOutcomes        = 4096
	outcomeFlushThreshold = 1024
)

type headEntry struct {
	outcomeID string
	revision  uint64
	sequence  uint64
}

type idemEntry struct {
	record   idemRecord
	sequence uint64
}

type keyAt struct {
	key      string
	sequence uint64
}

type State struct {
	mu       sync.RWMutex
	sequence uint64
	// lastHash is the journal hash of the record at sequence, which the
	// archive records beside its position.
	lastHash [32]byte
	// archive, when set, receives every Outcome; through is the position it
	// already held when this state started replaying.
	archive         Archive
	archivedThrough uint64
	heads           map[string]headEntry
	headOrder       []keyAt
	idempotency     map[string]idemEntry
	idemOrder       []keyAt
	queue           map[string]SnapshotRecord
}

func NewState() *State {
	return &State{heads: map[string]headEntry{}, idempotency: map[string]idemEntry{}, queue: map[string]SnapshotRecord{}}
}

// SetArchive sends Outcomes to archive. Records at or before through are
// already there and are not queued again. Without an archive the state keeps
// every Outcome and every head, as it did before Outcomes were archived.
func (s *State) SetArchive(archive Archive, through uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.archive, s.archivedThrough = archive, through
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
	if _, exists := s.queue[e.OutcomeID]; exists {
		return errors.New("outcome id already exists")
	}
	key := headKey(e.ProjectID, e.WorkUnitID, e.DefinitionID, e.DefinitionVersion)
	if head, known := s.heads[key]; known {
		if e.Revision != head.revision+1 {
			return errors.New("outcome revision is not contiguous")
		}
		if e.SupersedesOutcomeID != head.outcomeID {
			return ErrRevisionConflict
		}
	} else if s.archive == nil || e.Revision == 1 {
		if e.Revision != 1 {
			return errors.New("outcome revision is not contiguous")
		}
		if e.SupersedesOutcomeID != "" {
			return ErrRevisionConflict
		}
	}
	if e.Revision > domain.MaxOutcomeRevisions {
		return ErrRevisionLimit
	}
	outcome := domain.Outcome{
		ID: e.OutcomeID, ProjectID: e.ProjectID, WorkUnitID: e.WorkUnitID,
		DefinitionID: e.DefinitionID, DefinitionVersion: e.DefinitionVersion,
		Value: e.Value, ReporterKeyID: e.ReporterKeyID, EvidenceSHA256: e.EvidenceSHA256,
		EvidenceRef: e.EvidenceRef, ObservedAt: e.ObservedAt, IngestedAt: e.IngestedAt,
		SupersedesOutcomeID: e.SupersedesOutcomeID, Revision: e.Revision, GovernanceSequence: record.Sequence,
	}
	if s.archive == nil || record.Sequence > s.archivedThrough {
		s.queue[outcome.ID] = SnapshotRecord{Outcome: outcome, IdempotencyKeyHash: e.IdempotencyKeyHash, RequestFingerprint: e.RequestFingerprint}
	}
	s.heads[key] = headEntry{outcomeID: outcome.ID, revision: outcome.Revision, sequence: record.Sequence}
	s.headOrder = append(s.headOrder, keyAt{key: key, sequence: record.Sequence})
	idempotencyKey := idemKey(e.ProjectID, e.IdempotencyKeyHash)
	s.idempotency[idempotencyKey] = idemEntry{record: idemRecord{Fingerprint: e.RequestFingerprint, OutcomeID: outcome.ID}, sequence: record.Sequence}
	s.idemOrder = append(s.idemOrder, keyAt{key: idempotencyKey, sequence: record.Sequence})
	if s.archive != nil {
		s.headOrder = forgetOldest(s.heads, s.headOrder, func(entry headEntry) uint64 { return entry.sequence })
		s.idemOrder = forgetOldest(s.idempotency, s.idemOrder, func(entry idemEntry) uint64 { return entry.sequence })
	}
	s.sequence, s.lastHash = record.Sequence, record.Hash
	return nil
}

// forgetOldest drops the least recently touched entries past recentOutcomes.
// order holds one element per touch; an element whose sequence is no longer
// its key's latest is stale and skipped. It is compacted when stale elements
// make up most of it, so it stays proportional to the ring.
func forgetOldest[T any](entries map[string]T, order []keyAt, sequenceOf func(T) uint64) []keyAt {
	for len(entries) > recentOutcomes && len(order) > 0 {
		oldest := order[0]
		order = order[1:]
		if entry, ok := entries[oldest.key]; ok && sequenceOf(entry) == oldest.sequence {
			delete(entries, oldest.key)
		}
	}
	if len(order) > 2*recentOutcomes {
		live := order[:0:0]
		for _, item := range order {
			if entry, ok := entries[item.key]; ok && sequenceOf(entry) == item.sequence {
				live = append(live, item)
			}
		}
		order = live
	}
	return order
}

// QueueLen is how many Outcomes await the archive.
func (s *State) QueueLen() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.queue)
}

// FlushOutcomes writes the queue to the archive, with the position and
// journal hash it covers, and drops what was written.
func (s *State) FlushOutcomes() error {
	s.mu.RLock()
	archive := s.archive
	items := make([]SnapshotRecord, 0, len(s.queue))
	for _, item := range s.queue {
		items = append(items, item)
	}
	through, hash := s.sequence, s.lastHash
	s.mu.RUnlock()
	if archive == nil {
		return nil
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Outcome.GovernanceSequence < items[j].Outcome.GovernanceSequence })
	if err := archive.WriteOutcomes(items, through, hash); err != nil {
		return err
	}
	s.mu.Lock()
	for _, item := range items {
		delete(s.queue, item.Outcome.ID)
	}
	s.mu.Unlock()
	return nil
}

func (s *State) Outcome(id string) (domain.Outcome, bool, error) {
	s.mu.RLock()
	item, queued := s.queue[id]
	archive := s.archive
	s.mu.RUnlock()
	if queued {
		return item.Outcome, true, nil
	}
	if archive == nil {
		return domain.Outcome{}, false, nil
	}
	outcome, found, err := archive.ArchivedOutcome(id)
	if err != nil {
		return domain.Outcome{}, false, errors.Join(ledger.ErrArchiveUnavailable, err)
	}
	return outcome, found, nil
}

func (s *State) Current(projectID, workUnitID, definitionID string, version uint64) (domain.Outcome, bool, error) {
	s.mu.RLock()
	head, known := s.heads[headKey(projectID, workUnitID, definitionID, version)]
	archive := s.archive
	s.mu.RUnlock()
	if known {
		return s.Outcome(head.outcomeID)
	}
	if archive == nil {
		return domain.Outcome{}, false, nil
	}
	outcome, found, err := archive.ArchivedHead(projectID, workUnitID, definitionID, version)
	if err != nil {
		return domain.Outcome{}, false, errors.Join(ledger.ErrArchiveUnavailable, err)
	}
	return outcome, found, nil
}

func (s *State) Idempotency(projectID, hash string) (idemRecord, bool, error) {
	s.mu.RLock()
	entry, known := s.idempotency[idemKey(projectID, hash)]
	archive := s.archive
	s.mu.RUnlock()
	if known {
		return entry.record, true, nil
	}
	if archive == nil {
		return idemRecord{}, false, nil
	}
	fingerprint, outcomeID, found, err := archive.ArchivedOutcomeIdempotency(projectID, hash)
	if err != nil {
		return idemRecord{}, false, errors.Join(ledger.ErrArchiveUnavailable, err)
	}
	return idemRecord{Fingerprint: fingerprint, OutcomeID: outcomeID}, found, nil
}

func (s *State) Sequence() uint64 { s.mu.RLock(); defer s.mu.RUnlock(); return s.sequence }

// VisitOutcomes visits matching Outcomes in ID order after query.After,
// queued and archived alike, until visit returns false or an error.
func (s *State) VisitOutcomes(query OutcomeQuery, visit func(domain.Outcome) (bool, error)) error {
	match := func(outcome domain.Outcome) bool {
		return (query.ProjectID == "" || outcome.ProjectID == query.ProjectID) &&
			(query.WorkUnitID == "" || outcome.WorkUnitID == query.WorkUnitID) &&
			(query.DefinitionID == "" || outcome.DefinitionID == query.DefinitionID) &&
			outcome.ID > query.After
	}
	s.mu.RLock()
	var held []domain.Outcome
	for _, item := range s.queue {
		if match(item.Outcome) {
			held = append(held, item.Outcome)
		}
	}
	archive := s.archive
	s.mu.RUnlock()
	sort.Slice(held, func(i, j int) bool { return held[i].ID < held[j].ID })
	var archived func(func(domain.Outcome) (bool, error)) error
	if archive != nil {
		archived = func(emit func(domain.Outcome) (bool, error)) error {
			return archive.VisitArchivedOutcomes(query, emit)
		}
	}
	return ledger.MergeVisit(held, func(outcome domain.Outcome) string { return outcome.ID }, archived, visit)
}

func (s *State) Outcomes(projectID, workUnitID, definitionID string) []domain.Outcome {
	items, _ := s.OutcomesContext(context.Background(), projectID, workUnitID, definitionID)
	return items
}

// OutcomesContext reads every matching Outcome, in journal order. It is for
// the reads that need them all at once — a Work Unit's own, a summary, an
// export; a listing pages with VisitOutcomes instead.
func (s *State) OutcomesContext(ctx context.Context, projectID, workUnitID, definitionID string) ([]domain.Outcome, error) {
	items := make([]domain.Outcome, 0)
	err := s.VisitOutcomes(OutcomeQuery{ProjectID: projectID, WorkUnitID: workUnitID, DefinitionID: definitionID}, func(outcome domain.Outcome) (bool, error) {
		items = append(items, outcome)
		return true, ctx.Err()
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].GovernanceSequence < items[j].GovernanceSequence })
	return items, nil
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
	previous, replayed, err := m.state.Idempotency(input.ProjectID, input.Intent.IdempotencyKeyHash)
	if err != nil {
		return domain.Outcome{}, false, ErrUnavailable
	}
	if replayed {
		if previous.Fingerprint != input.Intent.RequestFingerprint {
			return domain.Outcome{}, false, ErrIdempotency
		}
		outcome, found, err := m.state.Outcome(previous.OutcomeID)
		if err != nil || !found {
			return domain.Outcome{}, false, ErrUnavailable
		}
		if outcome.ProjectID != input.ProjectID || outcome.WorkUnitID != input.WorkUnitID || outcome.DefinitionID != input.DefinitionID {
			return domain.Outcome{}, false, ErrIdempotency
		}
		return outcome, true, nil
	}
	workUnit, ok, err := m.accounting.WorkUnit(input.WorkUnitID)
	if err != nil {
		return domain.Outcome{}, false, ErrUnavailable
	}
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
	current, hasCurrent, err := m.state.Current(input.ProjectID, input.WorkUnitID, reference.ID, reference.Version)
	if err != nil {
		return domain.Outcome{}, false, ErrUnavailable
	}
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
	// The Outcome is in the journal; a failed archive write leaves it only in
	// the state's queue, which the next start rebuilds from the journal, so
	// the manager stops taking reports rather than let the queue grow.
	if err := m.state.FlushOutcomes(); err != nil {
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
	outcome, _, _ := m.state.Outcome(outcomeID)
	outcome.Provisional = workUnit.Status == domain.WorkUnitOpen || m.workUnitInflight(workUnit.ID)
	return outcome, false, nil
}

// workUnitInflight reports a request or attempt attributed to the Work Unit
// still in flight; one that cannot be read is reported in flight, which only
// marks its Outcomes provisional.
func (m *Manager) workUnitInflight(workUnitID string) bool {
	view, ok, err := m.accounting.WorkUnitView(workUnitID)
	return err != nil || !ok || view.Inflight
}

func (m *Manager) Outcomes(projectID, workUnitID, definitionID string) []domain.Outcome {
	items := m.state.Outcomes(projectID, workUnitID, definitionID)
	for index := range items {
		wu, ok, err := m.accounting.WorkUnit(items[index].WorkUnitID)
		items[index].Provisional = err != nil || !ok || wu.Status == domain.WorkUnitOpen || m.workUnitInflight(wu.ID)
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

// VisitOutcomes pages through Outcomes in ID order; see State.VisitOutcomes.
func (m *Manager) VisitOutcomes(query OutcomeQuery, visit func(domain.Outcome) (bool, error)) error {
	m.mu.Lock()
	unavailable := m.unavailable != nil || m.log == nil
	m.mu.Unlock()
	if unavailable {
		return ErrUnavailable
	}
	return m.state.VisitOutcomes(query, visit)
}
