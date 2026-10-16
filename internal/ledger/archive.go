package ledger

import (
	"container/heap"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

// Archiving finished Work Units (docs/todo/memory-bounded-redesign.zh-CN.md
// §13.2).
//
// A closed Work Unit would otherwise stay in the state for good, with its Runs,
// its cost totals and the idempotency records that point at it. It leaves once
// nothing can change it any more: it is closed, each of its Runs is closed or
// past its expiry, and no request or attempt attributed to it is in flight. The
// expiry is read against the OccurredAt of the record being applied, so a
// replay archives at exactly the records the live instance did.
//
// An archived Work Unit goes to a queue first and to the Archive only when the
// snapshot writer copies the queue, writes it durably and then the snapshot.
// It leaves the queue only after the Archive holds it, so a lookup finds it in
// one of the three places at every moment.

// ArchiveQueueLimit is how many archived Work Units the state holds before the
// snapshot writer is asked to write them out early.
const ArchiveQueueLimit = 4096

// ArchivedWorkUnit is one finished Work Unit with everything that left the
// state with it.
type ArchivedWorkUnit struct {
	WorkUnit    domain.WorkUnit         `json:"work_unit"`
	Cost        WorkUnitCost            `json:"cost"`
	Runs        []domain.Run            `json:"runs"`
	Idempotency []GovernanceIdempotency `json:"idempotency"`
	// Sequence is the record whose application archived it.
	Sequence uint64 `json:"sequence"`
}

// ArchiveQuery selects archived Work Units. After is exclusive and orders by
// ID; an empty ProjectID visits every project.
type ArchiveQuery struct {
	ProjectID   string
	CohortStart string
	CohortEnd   string
	After       string
}

// Archive is where archived Work Units are kept. Reads that fail return an
// error and are never taken for "absent": an idempotent retry read as absent
// would be executed a second time.
type Archive interface {
	ArchivedWorkUnit(id string) (ArchivedWorkUnit, bool, error)
	ArchivedRun(id string) (domain.Run, bool, error)
	ArchivedIdempotency(projectID, operation, keyHash string) (GovernanceIdempotency, bool, error)
	// VisitArchivedWorkUnits visits matching Work Units in ID order until
	// visit returns false or an error.
	VisitArchivedWorkUnits(query ArchiveQuery, visit func(ArchivedWorkUnit) (bool, error)) error
	// VisitArchivedRuns visits a project's Runs (every project's when
	// projectID is empty) in ID order after the cursor.
	VisitArchivedRuns(projectID, after string, visit func(domain.Run) (bool, error)) error
	// WriteArchived stores items durably and records that every Work Unit
	// archived at or before through is stored, together with the Ledger's
	// chain head at or past through, which ties the archive to one history.
	WriteArchived(items []ArchivedWorkUnit, through uint64, chain StateSnapshotChain) error
	// ArchivedThrough is the position and chain head WriteArchived last
	// recorded.
	ArchivedThrough() (uint64, StateSnapshotChain, error)
	// ResetArchived discards every archived Work Unit and the recorded
	// position, before a rebuild from the first frame writes them again.
	ResetArchived() error
}

// SetArchive enables archiving into archive. It is set once, before the first
// record is applied; a state without one keeps every Work Unit, which is what
// offline tools that replay the Ledger once want.
func (s *State) SetArchive(archive Archive) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.archive = archive
}

// archiveIndexes are the parts of the state that only make archiving and its
// lookups cheap. They are derived from the rest and never written to a
// snapshot.
type archiveIndexes struct {
	// queue holds archived Work Units the Archive may not hold yet, with the
	// IDs of their Runs and idempotency records for lookups.
	queue         map[string]ArchivedWorkUnit
	queuedRuns    map[string]string
	queuedIdem    map[string]string
	idemByUnit    map[string][]string
	openByProject map[string]map[string]struct{}
	liveRuns      map[string]map[string]struct{}
	// expiries holds closed Work Units waiting on a Run to expire, each at
	// most once: waiting names those on it. Every record attributed to such a
	// Work Unit looks at it again, and would otherwise add an entry each time.
	expiries expiryHeap
	waiting  map[string]struct{}
}

func newArchiveIndexes() archiveIndexes {
	return archiveIndexes{
		queue: map[string]ArchivedWorkUnit{}, queuedRuns: map[string]string{}, queuedIdem: map[string]string{},
		idemByUnit: map[string][]string{}, openByProject: map[string]map[string]struct{}{},
		liveRuns: map[string]map[string]struct{}{}, waiting: map[string]struct{}{},
	}
}

type expiry struct {
	at         time.Time
	workUnitID string
}

type expiryHeap []expiry

func (h expiryHeap) Len() int           { return len(h) }
func (h expiryHeap) Less(i, j int) bool { return h[i].at.Before(h[j].at) }
func (h expiryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *expiryHeap) Push(x any)        { *h = append(*h, x.(expiry)) }
func (h *expiryHeap) Pop() any {
	old := *h
	item := old[len(old)-1]
	*h = old[:len(old)-1]
	return item
}

func addMember(sets map[string]map[string]struct{}, key, member string) {
	if sets[key] == nil {
		sets[key] = map[string]struct{}{}
	}
	sets[key][member] = struct{}{}
}

func removeMember(sets map[string]map[string]struct{}, key, member string) {
	delete(sets[key], member)
	if len(sets[key]) == 0 {
		delete(sets, key)
	}
}

// rebuildArchiveIndexes derives the indexes from the Work Units, Runs and
// idempotency records the state holds, after a snapshot is restored. A closed
// Work Unit still held is put back on the expiry heap, so it is examined again
// at the next record.
func (s *State) rebuildArchiveIndexes() {
	s.indexes = newArchiveIndexes()
	for key, record := range s.governanceIdempotency {
		s.indexes.idemByUnit[record.WorkUnitID] = append(s.indexes.idemByUnit[record.WorkUnitID], key)
	}
	for _, keys := range s.indexes.idemByUnit {
		sort.Strings(keys)
	}
	for _, workUnit := range s.workUnits {
		if workUnit.Status == domain.WorkUnitOpen {
			addMember(s.indexes.openByProject, workUnit.ProjectID, workUnit.ID)
		} else {
			s.awaitExpiry(workUnit.ID, time.Time{})
		}
	}
	for _, run := range s.runs {
		if run.Status == domain.RunActive {
			addMember(s.indexes.liveRuns, run.ProjectID, run.ID)
		}
	}
}

// noteGovernanceEvent keeps the indexes in step with one applied governance
// record. It runs after the record has changed the maps.
func (s *State) noteGovernanceEvent(event Event, idempotencyKey string) {
	s.indexes.idemByUnit[event.WorkUnitID] = append(s.indexes.idemByUnit[event.WorkUnitID], idempotencyKey)
	switch event.Kind {
	case EventWorkUnitCreated:
		addMember(s.indexes.openByProject, event.ProjectID, event.WorkUnitID)
	case EventWorkUnitClosed:
		removeMember(s.indexes.openByProject, event.ProjectID, event.WorkUnitID)
	case EventRunCreated:
		addMember(s.indexes.liveRuns, event.ProjectID, event.RunID)
	case EventRunClosed:
		removeMember(s.indexes.liveRuns, event.ProjectID, event.RunID)
	}
}

// archiveFinished archives what the record at sequence may have finished: the
// Work Unit it names, and any closed Work Unit whose last Run has expired by
// the record's clock.
func (s *State) archiveFinished(workUnitID string, now time.Time, sequence uint64) {
	if s.archive == nil {
		return
	}
	if workUnitID != "" {
		s.tryArchive(workUnitID, now, sequence)
	}
	for len(s.indexes.expiries) > 0 && !now.Before(s.indexes.expiries[0].at) {
		item := heap.Pop(&s.indexes.expiries).(expiry)
		delete(s.indexes.waiting, item.workUnitID)
		s.tryArchive(item.workUnitID, now, sequence)
	}
}

// awaitExpiry puts a closed Work Unit on the expiry heap unless it is already
// there. An entry earlier than the Work Unit's latest expiry is harmless: when
// it is popped, tryArchive looks again and puts it back for the later one.
func (s *State) awaitExpiry(workUnitID string, at time.Time) {
	if _, ok := s.indexes.waiting[workUnitID]; ok {
		return
	}
	s.indexes.waiting[workUnitID] = struct{}{}
	heap.Push(&s.indexes.expiries, expiry{at: at, workUnitID: workUnitID})
}

func (s *State) tryArchive(workUnitID string, now time.Time, sequence uint64) {
	workUnit, ok := s.workUnits[workUnitID]
	if !ok || workUnit.Status != domain.WorkUnitClosed {
		return
	}
	if len(s.activeByWorkUnit[workUnitID]) > 0 || len(s.pendingByWorkUnit[workUnitID]) > 0 {
		return // a settlement or finalization will look again
	}
	var latest time.Time
	for _, runID := range s.runsByWorkUnit[workUnitID] {
		run := s.runs[runID]
		if run.Status == domain.RunActive && now.Before(run.ExpiresAt) && run.ExpiresAt.After(latest) {
			latest = run.ExpiresAt
		}
	}
	if !latest.IsZero() {
		s.awaitExpiry(workUnitID, latest)
		return
	}
	item := ArchivedWorkUnit{WorkUnit: workUnit, Cost: s.workUnitCosts[workUnitID], Sequence: sequence}
	for _, runID := range s.runsByWorkUnit[workUnitID] {
		run := s.runs[runID]
		item.Runs = append(item.Runs, run)
		s.indexes.queuedRuns[runID] = workUnitID
		removeMember(s.indexes.liveRuns, run.ProjectID, runID)
		delete(s.runs, runID)
	}
	// Sorted, so the item does not depend on the order the records were
	// applied in, which a restored snapshot does not keep.
	keys := slices.Sorted(slices.Values(s.indexes.idemByUnit[workUnitID]))
	for _, key := range keys {
		if record, ok := s.governanceIdempotency[key]; ok {
			item.Idempotency = append(item.Idempotency, record)
			s.indexes.queuedIdem[key] = workUnitID
			delete(s.governanceIdempotency, key)
		}
	}
	s.indexes.queue[workUnitID] = item
	delete(s.indexes.idemByUnit, workUnitID)
	delete(s.runsByWorkUnit, workUnitID)
	delete(s.workUnitCosts, workUnitID)
	delete(s.workUnits, workUnitID)
}

// ArchiveQueueLen is how many archived Work Units await the Archive.
func (s *State) ArchiveQueueLen() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.indexes.queue)
}

// copyArchiveQueue copies the queue. The caller holds the lock.
func (s *State) copyArchiveQueue() []ArchivedWorkUnit {
	items := make([]ArchivedWorkUnit, 0, len(s.indexes.queue))
	for _, item := range s.indexes.queue {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Sequence < items[j].Sequence })
	return items
}

// forgetArchived drops written items from the queue. An item archived again
// since the copy — impossible, an ID is archived once — is left alone by
// comparing sequences.
func (s *State) forgetArchived(items []ArchivedWorkUnit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range items {
		queued, ok := s.indexes.queue[item.WorkUnit.ID]
		if !ok || queued.Sequence != item.Sequence {
			continue
		}
		for _, run := range queued.Runs {
			delete(s.indexes.queuedRuns, run.ID)
		}
		for _, record := range queued.Idempotency {
			delete(s.indexes.queuedIdem, governanceIdempotencyMapKey(record.ProjectID, record.Operation, record.KeyHash))
		}
		delete(s.indexes.queue, item.WorkUnit.ID)
	}
}

// FlushArchive writes the queue to the Archive as of the state's position and
// drops what was written. A start rebuilding the state from the first frame
// calls it as the queue fills, so the rebuild holds no more than a queue's
// worth; a running instance's snapshot writer does the same before each
// snapshot. The chain head is read after the queue is copied, so it is at or
// past the position.
func (s *State) FlushArchive(log ChainHeadReader) error {
	s.mu.RLock()
	archive := s.archive
	items := s.copyArchiveQueue()
	through := s.watermark.Sequence
	s.mu.RUnlock()
	if archive == nil {
		return nil
	}
	head, hash, ok := log.ChainHead()
	if !ok || head.Sequence < through {
		return errors.New("the Ledger has no chain head covering the archive")
	}
	if err := archive.WriteArchived(items, through, StateSnapshotChain{Sequence: head.Sequence, Hash: hash}); err != nil {
		return err
	}
	s.forgetArchived(items)
	return nil
}

// ErrArchiveUnavailable is a read of archived state that failed. It is never
// taken for "absent".
var ErrArchiveUnavailable = errors.New("archived state is unavailable")

// lookupArchived finds a Work Unit that has left the state. The caller holds
// no lock: the Archive is read outside it.
func (s *State) lookupArchived(id string) (ArchivedWorkUnit, bool, error) {
	s.mu.RLock()
	item, queued := s.indexes.queue[id]
	archive := s.archive
	s.mu.RUnlock()
	if queued {
		return item, true, nil
	}
	if archive == nil {
		return ArchivedWorkUnit{}, false, nil
	}
	item, found, err := archive.ArchivedWorkUnit(id)
	if err != nil {
		return ArchivedWorkUnit{}, false, errors.Join(ErrArchiveUnavailable, err)
	}
	return item, found, nil
}

func (s *State) WorkUnit(id string) (domain.WorkUnit, bool, error) {
	s.mu.RLock()
	value, ok := s.workUnits[id]
	s.mu.RUnlock()
	if ok {
		value.OutcomeDefinitions = slices.Clone(value.OutcomeDefinitions)
		return value, true, nil
	}
	item, found, err := s.lookupArchived(id)
	return item.WorkUnit, found, err
}

func (s *State) Run(id string) (domain.Run, bool, error) {
	s.mu.RLock()
	value, ok := s.runs[id]
	workUnitID, queued := s.indexes.queuedRuns[id]
	item := s.indexes.queue[workUnitID]
	archive := s.archive
	s.mu.RUnlock()
	if ok {
		return value, true, nil
	}
	if queued {
		for _, run := range item.Runs {
			if run.ID == id {
				return run, true, nil
			}
		}
	}
	if archive == nil {
		return domain.Run{}, false, nil
	}
	run, found, err := archive.ArchivedRun(id)
	if err != nil {
		return domain.Run{}, false, errors.Join(ErrArchiveUnavailable, err)
	}
	return run, found, nil
}

func (s *State) GovernanceIdempotency(projectID, operation, keyHash string) (GovernanceIdempotency, bool, error) {
	key := governanceIdempotencyMapKey(projectID, operation, keyHash)
	s.mu.RLock()
	value, ok := s.governanceIdempotency[key]
	workUnitID, queued := s.indexes.queuedIdem[key]
	item := s.indexes.queue[workUnitID]
	archive := s.archive
	s.mu.RUnlock()
	if ok {
		return value, true, nil
	}
	if queued {
		for _, record := range item.Idempotency {
			if governanceIdempotencyMapKey(record.ProjectID, record.Operation, record.KeyHash) == key {
				return record, true, nil
			}
		}
	}
	if archive == nil {
		return GovernanceIdempotency{}, false, nil
	}
	record, found, err := archive.ArchivedIdempotency(projectID, operation, keyHash)
	if err != nil {
		return GovernanceIdempotency{}, false, errors.Join(ErrArchiveUnavailable, err)
	}
	return record, found, nil
}

// OpenWorkUnitCount is how many of the project's Work Units are open.
func (s *State) OpenWorkUnitCount(projectID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.indexes.openByProject[projectID])
}

// LiveRuns is the project's Runs that are not closed, expired ones included;
// none of them has been archived.
func (s *State) LiveRuns(projectID string) []domain.Run {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]domain.Run, 0, len(s.indexes.liveRuns[projectID]))
	for runID := range s.indexes.liveRuns[projectID] {
		items = append(items, s.runs[runID])
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

// WorkUnitRunCount is how many Runs the Work Unit has had. Runs are created
// only on an open Work Unit, and an open one is never archived.
func (s *State) WorkUnitRunCount(workUnitID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.runsByWorkUnit[workUnitID])
}

// WorkUnitView is a Work Unit with its Runs and cost totals, wherever it is
// held.
type WorkUnitView struct {
	WorkUnit domain.WorkUnit
	Runs     []domain.Run
	Cost     WorkUnitCost
	// Inflight reports a request or attempt attributed to it still in flight.
	Inflight bool
}

// heldViews copies the Work Units the state and its queue hold that match,
// sorted by ID. The caller holds the read lock.
func (s *State) heldViews(match func(domain.WorkUnit) bool) []WorkUnitView {
	var views []WorkUnitView
	for _, workUnit := range s.workUnits {
		if !match(workUnit) {
			continue
		}
		view := WorkUnitView{WorkUnit: workUnit, Cost: s.workUnitCosts[workUnit.ID],
			Inflight: len(s.activeByWorkUnit[workUnit.ID]) > 0 || len(s.pendingByWorkUnit[workUnit.ID]) > 0}
		view.WorkUnit.OutcomeDefinitions = slices.Clone(workUnit.OutcomeDefinitions)
		for _, runID := range s.runsByWorkUnit[workUnit.ID] {
			view.Runs = append(view.Runs, s.runs[runID])
		}
		views = append(views, view)
	}
	for _, item := range s.indexes.queue {
		if match(item.WorkUnit) {
			views = append(views, WorkUnitView{WorkUnit: item.WorkUnit, Runs: slices.Clone(item.Runs), Cost: item.Cost})
		}
	}
	sort.Slice(views, func(i, j int) bool { return views[i].WorkUnit.ID < views[j].WorkUnit.ID })
	return views
}

// VisitWorkUnits visits matching Work Units in ID order — those the state
// holds and those archived — until visit returns false or an error.
func (s *State) VisitWorkUnits(query ArchiveQuery, visit func(WorkUnitView) (bool, error)) error {
	match := func(workUnit domain.WorkUnit) bool {
		return (query.ProjectID == "" || workUnit.ProjectID == query.ProjectID) &&
			(query.CohortStart == "" || workUnit.PeriodID >= query.CohortStart && workUnit.PeriodID <= query.CohortEnd) &&
			workUnit.ID > query.After
	}
	s.mu.RLock()
	held := s.heldViews(match)
	archive := s.archive
	s.mu.RUnlock()
	var archived func(func(WorkUnitView) (bool, error)) error
	if archive != nil {
		archived = func(emit func(WorkUnitView) (bool, error)) error {
			return archive.VisitArchivedWorkUnits(query, func(item ArchivedWorkUnit) (bool, error) {
				return emit(WorkUnitView{WorkUnit: item.WorkUnit, Runs: item.Runs, Cost: item.Cost})
			})
		}
	}
	return MergeVisit(held, func(view WorkUnitView) string { return view.WorkUnit.ID }, archived, visit)
}

// VisitRuns visits a project's Runs (every project's when projectID is
// empty) in ID order after the cursor, held and archived alike.
func (s *State) VisitRuns(projectID, after string, visit func(domain.Run) (bool, error)) error {
	s.mu.RLock()
	var held []domain.Run
	for _, run := range s.runs {
		if (projectID == "" || run.ProjectID == projectID) && run.ID > after {
			held = append(held, run)
		}
	}
	for _, item := range s.indexes.queue {
		for _, run := range item.Runs {
			if (projectID == "" || run.ProjectID == projectID) && run.ID > after {
				held = append(held, run)
			}
		}
	}
	archive := s.archive
	s.mu.RUnlock()
	sort.Slice(held, func(i, j int) bool { return held[i].ID < held[j].ID })
	var archived func(func(domain.Run) (bool, error)) error
	if archive != nil {
		archived = func(emit func(domain.Run) (bool, error)) error {
			return archive.VisitArchivedRuns(projectID, after, emit)
		}
	}
	return MergeVisit(held, func(run domain.Run) string { return run.ID }, archived, visit)
}

// MergeVisit visits held items (sorted by id) and archived ones (visited by
// archived in id order) as one ordered sequence, each id once. The held items
// are copied under the owner's lock before the archive is read, so an item
// archived in between is in the copy, and one in both is visited from the
// copy. A nil archived visits the held items alone. An error from the archive
// is wrapped as ErrArchiveUnavailable; one from visit is returned as it is.
func MergeVisit[T any](held []T, id func(T) string, archived func(func(T) (bool, error)) error, visit func(T) (bool, error)) error {
	seen := make(map[string]struct{}, len(held))
	for _, item := range held {
		seen[id(item)] = struct{}{}
	}
	index := 0
	emitHeldBefore := func(bound string) (bool, error) {
		for index < len(held) && (bound == "" || id(held[index]) < bound) {
			more, err := visit(held[index])
			index++
			if err != nil || !more {
				return false, err
			}
		}
		return true, nil
	}
	if archived != nil {
		// visitErr keeps the caller's own error apart from the Archive's, so
		// a canceled request is not reported as an unavailable Archive.
		stopped := false
		var visitErr error
		err := archived(func(item T) (bool, error) {
			if _, dup := seen[id(item)]; dup {
				return true, nil
			}
			more, err := emitHeldBefore(id(item))
			if err == nil && more {
				more, err = visit(item)
			}
			if err != nil || !more {
				stopped, visitErr = true, err
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			return errors.Join(ErrArchiveUnavailable, err)
		}
		if stopped {
			return visitErr
		}
	}
	_, err := emitHeldBefore("")
	return err
}

// workUnitView finds one Work Unit wherever it is held.
func (s *State) workUnitView(id string) (WorkUnitView, bool, error) {
	s.mu.RLock()
	held := s.heldViews(func(workUnit domain.WorkUnit) bool { return workUnit.ID == id })
	s.mu.RUnlock()
	if len(held) > 0 {
		return held[0], true, nil
	}
	item, found, err := s.lookupArchived(id)
	if err != nil || !found {
		return WorkUnitView{}, false, err
	}
	return WorkUnitView{WorkUnit: item.WorkUnit, Runs: item.Runs, Cost: item.Cost}, true, nil
}

// WorkUnitView finds one Work Unit with its Runs and cost totals.
func (s *State) WorkUnitView(id string) (WorkUnitView, bool, error) {
	return s.workUnitView(id)
}

// HoldsRun reports that the Run is still in the state rather than archived.
func (s *State) HoldsRun(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.runs[id]
	return ok
}
