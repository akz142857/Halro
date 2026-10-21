package replication

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"time"
)

const maxLiveTransitionEvents = 128

// MemberTransitionEvent is emitted only after an authenticated role/term state
// transition was durably published. It is process-local operational evidence,
// not an Audit record or proof that earlier process lifetimes were observed.
type MemberTransitionEvent struct {
	Sequence         uint64    `json:"sequence"`
	At               time.Time `json:"at"`
	Kind             string    `json:"kind"`
	FromRole         Role      `json:"from_role"`
	ToRole           Role      `json:"to_role"`
	FromTerm         uint64    `json:"from_term"`
	ToTerm           uint64    `json:"to_term"`
	FromPromisedTerm uint64    `json:"from_promised_term"`
	ToPromisedTerm   uint64    `json:"to_promised_term"`
}

type LiveTransitionHistory struct {
	PublisherStartedAt time.Time               `json:"publisher_started_at"`
	Dropped            uint64                  `json:"dropped"`
	Events             []MemberTransitionEvent `json:"events"`
}

// StatePublisher serializes progress and role transitions into one authenticated
// state.json snapshot. Every transition uses the same file-fsync, rename and
// directory-fsync barrier as progress so a promise cannot be acknowledged
// before it survives a crash.
type StatePublisher struct {
	mu                     sync.Mutex
	path                   string
	key                    [sha256.Size]byte
	state                  MemberState
	journal                *TransitionJournal
	poisoned               bool
	startedAt              time.Time
	transitions            []MemberTransitionEvent
	droppedTransitions     uint64
	nextTransitionSequence uint64
}

func NewStatePublisher(path string, key []byte, state MemberState) (*StatePublisher, error) {
	if path == "" || len(key) != sha256.Size {
		return nil, errors.New("member-state publisher requires a path and 32-byte key")
	}
	if err := state.Validate(); err != nil {
		return nil, err
	}
	if state.Version == TransitionStateVersion {
		return nil, errors.New("version-3 member state requires the durable transition journal before publication")
	}
	if _, err := os.Lstat(TransitionJournalPath(path)); err == nil {
		return nil, errors.New("version-2 member state has a transition journal; finish or recover the offline migration")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect transition journal before version-2 publication: %w", err)
	}
	segments, err := transitionSegments(TransitionJournalPath(path))
	if err != nil || len(segments) != 1 {
		return nil, errors.New("version-2 member state has an orphaned transition journal segment; recover before publication")
	}
	publisher := &StatePublisher{path: path, state: cloneMemberState(state), startedAt: time.Now().UTC()}
	copy(publisher.key[:], key)
	return publisher, nil
}

// TransitionJournalPath is the member-local durable evidence file. It must
// remain in the same private cluster directory as state.json.
func TransitionJournalPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "transitions.journal")
}

// NewDurableStatePublisher opens version-3 state only after its authenticated
// journal has been reconciled. Existing version-2 runtime paths do not call
// this constructor until an offline migration and rollout gate are complete.
func NewDurableStatePublisher(path string, key []byte, state MemberState) (*StatePublisher, error) {
	if path == "" || len(key) != sha256.Size || state.Version != TransitionStateVersion {
		return nil, errors.New("durable state publisher requires a version-3 state and 32-byte key")
	}
	if err := state.Validate(); err != nil {
		return nil, err
	}
	onDisk, err := ReadState(path, key)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(onDisk, state) {
		return nil, errors.New("durable state publisher input differs from authenticated state file")
	}
	journal, err := OpenTransitionJournal(TransitionJournalPath(path), key, state)
	if err != nil {
		return nil, err
	}
	if err := journal.ResolvePending(); err != nil {
		_ = journal.Close()
		return nil, err
	}
	publisher := &StatePublisher{path: path, state: cloneMemberState(state), journal: journal, startedAt: time.Now().UTC()}
	copy(publisher.key[:], key)
	return publisher, nil
}

// OpenStatePublisher selects the publication protocol already authenticated
// in state.json. It never performs a version migration during normal startup.
func OpenStatePublisher(path string, key []byte, state MemberState) (*StatePublisher, error) {
	if state.Version == TransitionStateVersion {
		return NewDurableStatePublisher(path, key, state)
	}
	return NewStatePublisher(path, key, state)
}

func (p *StatePublisher) Snapshot() MemberState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneMemberState(p.state)
}

func (p *StatePublisher) LiveTransitions() LiveTransitionHistory {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.liveTransitionsLocked()
}

func (p *StatePublisher) SnapshotWithLiveTransitions() (MemberState, LiveTransitionHistory) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneMemberState(p.state), p.liveTransitionsLocked()
}

func (p *StatePublisher) liveTransitionsLocked() LiveTransitionHistory {
	return LiveTransitionHistory{
		PublisherStartedAt: p.startedAt, Dropped: p.droppedTransitions,
		Events: append([]MemberTransitionEvent(nil), p.transitions...),
	}
}

// Close erases the in-memory cluster authentication key after the runtime has
// stopped publishing member progress. StatePublisher owns its private copy.
func (p *StatePublisher) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.key[:])
	if p.journal != nil {
		_ = p.journal.Close()
	}
}

// DurableTransitions returns committed member role/term transitions. The
// process-local LiveTransitions API remains a separate bounded source.
func (p *StatePublisher) DurableTransitions() ([]MemberTransitionEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.journal == nil {
		return nil, errors.New("durable transitions are not configured for this member")
	}
	return p.journal.CommittedEvents()
}

func (p *StatePublisher) DurableTransitionPage(after uint64, digest string, limit int) (DurableTransitionPage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.journal == nil {
		return DurableTransitionPage{}, errors.New("durable transitions are not configured for this member")
	}
	return p.journal.Page(after, digest, limit)
}

// TransitionJournalStorage reports member-local retained journal files. A
// version-2 member has no journal, so callers must treat that as unavailable
// rather than as a zero-sized version-3 history.
func (p *StatePublisher) TransitionJournalStorage() (TransitionJournalStorage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.journal == nil {
		return TransitionJournalStorage{}, errors.New("durable transitions are not configured for this member")
	}
	return p.journal.storage()
}

func (p *StatePublisher) PublishPrimary(progress PrimaryProgress) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.Role != RolePrimary {
		return errors.New("cannot publish Primary progress for a non-Primary member")
	}
	if progress.DurableIndex < p.state.DurableIndex || progress.ConfirmedIndex < p.state.ConfirmedIndex ||
		progress.ConfirmedIndex > progress.DurableIndex {
		return errors.New("primary member-state progress regressed or crossed its durable prefix")
	}
	if progress.Projection.Index != progress.ConfirmedIndex ||
		progress.Projection.MetadataEpoch < p.state.Projection.MetadataEpoch ||
		progress.Projection.MetadataEpoch == p.state.Projection.MetadataEpoch && progress.Projection.MetadataSequence < p.state.Projection.MetadataSequence {
		return errors.New("primary member-state projection regressed or does not match confirmed progress")
	}
	if progress.DurableIndex == 0 && progress.OrderingHeadMAC != ([sha256.Size]byte{}) ||
		progress.DurableIndex > 0 && progress.OrderingHeadMAC == ([sha256.Size]byte{}) {
		return errors.New("primary member-state progress has an invalid ordering head")
	}
	next := cloneMemberState(p.state)
	next.DurableIndex = progress.DurableIndex
	next.ConfirmedIndex = progress.ConfirmedIndex
	// Primary native stores apply before ordering/confirmation. The durable
	// state file exposes only the quorum-safe applied prefix, so promotion and
	// observability never mistake an unconfirmed local suffix for transferable
	// authority.
	next.AppliedIndex = progress.ConfirmedIndex
	next.OrderingHeadMAC = progress.OrderingHeadMAC
	next.Projection = progress.Projection
	return p.publish(next)
}

func (p *StatePublisher) PublishReplica(progress ReplicaProgress) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.Role != RoleReplica {
		return errors.New("cannot publish Replica progress for a non-Replica member")
	}
	if progress.DurableIndex < p.state.DurableIndex || progress.ConfirmedIndex < p.state.ConfirmedIndex ||
		progress.AppliedIndex < p.state.AppliedIndex || progress.ConfirmedIndex > progress.DurableIndex ||
		progress.AppliedIndex > progress.ConfirmedIndex {
		return errors.New("replica member-state progress regressed or crossed its allowed prefix")
	}
	if progress.Projection.Index != progress.AppliedIndex ||
		progress.Projection.MetadataEpoch < p.state.Projection.MetadataEpoch ||
		progress.Projection.MetadataEpoch == p.state.Projection.MetadataEpoch && progress.Projection.MetadataSequence < p.state.Projection.MetadataSequence {
		return errors.New("replica member-state projection regressed or does not match applied progress")
	}
	if progress.DurableIndex == 0 && progress.OrderingHeadMAC != ([sha256.Size]byte{}) ||
		progress.DurableIndex > 0 && progress.OrderingHeadMAC == ([sha256.Size]byte{}) {
		return errors.New("replica member-state progress has an invalid ordering head")
	}
	next := cloneMemberState(p.state)
	next.DurableIndex = progress.DurableIndex
	next.ConfirmedIndex = progress.ConfirmedIndex
	next.AppliedIndex = progress.AppliedIndex
	next.OrderingHeadMAC = progress.OrderingHeadMAC
	next.Projection = progress.Projection
	return p.publish(next)
}

// Promise persists a strictly newer term before the caller returns an answer
// to a prepare request. A Primary becomes a Replica in the same publication:
// after making the promise it may neither confirm the old term nor initiate a
// new Provider side effect.
func (p *StatePublisher) Promise(term uint64) (MemberState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if term <= p.state.PromisedTerm {
		return MemberState{}, errors.New("proposed term must be greater than promised_term")
	}
	next := cloneMemberState(p.state)
	next.PromisedTerm = term
	if next.Role == RolePrimary {
		next.Role = RoleReplica
	}
	if err := p.publishTransition(next, "promise"); err != nil {
		return MemberState{}, err
	}
	return cloneMemberState(p.state), nil
}

// Promote commits an already-promised term locally. The caller must have
// completed promise collection, fencing and freshness checks before invoking
// it; those inputs are explicit so a stale operator command cannot promote a
// different prefix than the one it inspected.
func (p *StatePublisher) Promote(expectedTerm, expectedAppliedIndex, term uint64) (MemberState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.Role != RoleReplica {
		return MemberState{}, errors.New("only a Replica may be promoted")
	}
	if p.state.Term != expectedTerm || p.state.AppliedIndex != expectedAppliedIndex {
		return MemberState{}, errors.New("member term or applied index changed after promotion inspection")
	}
	if term != p.state.PromisedTerm || term <= p.state.Term {
		return MemberState{}, errors.New("promotion term must equal a durable promise newer than the current term")
	}
	next := cloneMemberState(p.state)
	next.Role = RolePrimary
	next.Term = term
	if err := p.publishTransition(next, "promote"); err != nil {
		return MemberState{}, err
	}
	return cloneMemberState(p.state), nil
}

// AdoptHigherTerm records startup adjudication against a peer that is already
// authoritative for a newer term. It never promotes and never moves data
// watermarks; suffix truncation/reseed happens while stores are closed before
// the next role-specific runtime is constructed.
func (p *StatePublisher) AdoptHigherTerm(term, promisedTerm uint64) (MemberState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if term < p.state.Term || promisedTerm < term || promisedTerm < p.state.PromisedTerm {
		return MemberState{}, errors.New("higher-term adoption would regress term or promised_term")
	}
	if term == p.state.Term && promisedTerm == p.state.PromisedTerm && p.state.Role == RoleReplica {
		return cloneMemberState(p.state), nil
	}
	next := cloneMemberState(p.state)
	next.Role = RoleReplica
	next.Term = term
	next.PromisedTerm = promisedTerm
	if err := p.publishTransition(next, "adopt_higher_term"); err != nil {
		return MemberState{}, err
	}
	return cloneMemberState(p.state), nil
}

func (p *StatePublisher) publish(next MemberState) error {
	if p.poisoned {
		return errors.New("member-state publisher requires restart after ambiguous durable write")
	}
	if !sameMemberIdentity(p.state, next) {
		return errors.New("member-state progress publication cannot change identity, role, term or membership")
	}
	if err := WriteState(p.path, next, p.key[:]); err != nil {
		p.poisoned = true
		return err
	}
	p.state = next
	return nil
}

func (p *StatePublisher) publishTransition(next MemberState, kind string) error {
	if p.poisoned {
		return errors.New("member-state publisher requires restart after ambiguous durable write")
	}
	if p.state.Version != next.Version || p.state.ClusterID != next.ClusterID || p.state.Incarnation != next.Incarnation ||
		p.state.NodeID != next.NodeID || !slices.Equal(p.state.Peers, next.Peers) {
		return errors.New("member-state transition cannot change identity or membership")
	}
	if next.DurableIndex != p.state.DurableIndex || next.ConfirmedIndex != p.state.ConfirmedIndex ||
		next.AppliedIndex != p.state.AppliedIndex || next.OrderingHeadMAC != p.state.OrderingHeadMAC ||
		next.Projection != p.state.Projection {
		return errors.New("member-state transition cannot change progress")
	}
	if next.Transition != p.state.Transition {
		return errors.New("member-state transition cannot set its own journal cursor")
	}
	if p.journal != nil {
		cursor, err := p.journal.appendIntent(kind, next)
		if err != nil {
			p.poisoned = true
			return err
		}
		next.Transition = cursor
	}
	if err := WriteState(p.path, next, p.key[:]); err != nil {
		p.poisoned = true
		return err
	}
	if p.journal != nil {
		if err := p.journal.commit(next.Transition); err != nil {
			p.poisoned = true
			return err
		}
	}
	previous := p.state
	p.state = next
	p.nextTransitionSequence++
	if len(p.transitions) == maxLiveTransitionEvents {
		copy(p.transitions, p.transitions[1:])
		p.transitions = p.transitions[:maxLiveTransitionEvents-1]
		p.droppedTransitions++
	}
	p.transitions = append(p.transitions, MemberTransitionEvent{
		Sequence: p.nextTransitionSequence, At: time.Now().UTC(), Kind: kind,
		FromRole: previous.Role, ToRole: next.Role, FromTerm: previous.Term, ToTerm: next.Term,
		FromPromisedTerm: previous.PromisedTerm, ToPromisedTerm: next.PromisedTerm,
	})
	return nil
}

func cloneMemberState(state MemberState) MemberState {
	state.Peers = append([]StatePeer(nil), state.Peers...)
	return state
}

func sameMemberIdentity(left, right MemberState) bool {
	return left.Version == right.Version && left.ClusterID == right.ClusterID && left.Incarnation == right.Incarnation &&
		left.NodeID == right.NodeID && left.Role == right.Role && left.Term == right.Term && left.PromisedTerm == right.PromisedTerm &&
		left.Transition == right.Transition &&
		slices.Equal(left.Peers, right.Peers)
}
