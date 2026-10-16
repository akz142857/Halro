package ledger

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/durable"
)

// StateSnapshotVersion is the format of a state snapshot. A snapshot of any
// other version is not read: the state is rebuilt from the Ledger instead.
const StateSnapshotVersion = 2

var (
	stateSnapshotMACLabel = []byte("halro:ledger-hot-state:v1\x00")
	// ErrStateSnapshotUnusable says a snapshot exists but cannot be trusted
	// or read: another version, a failed MAC, a damaged file. The caller
	// rebuilds the state from the Ledger.
	ErrStateSnapshotUnusable = errors.New("ledger state snapshot is unusable")
)

// stateSnapshot is the state as it is written to disk. Every map becomes a
// slice in a fixed order, so the same state always encodes to the same bytes.
type stateSnapshot struct {
	Version               int                     `json:"version"`
	Watermark             Watermark               `json:"watermark"`
	Chain                 StateSnapshotChain      `json:"chain"`
	Balances              []snapshotBalance       `json:"balances"`
	Reservations          []snapshotReservation   `json:"reservations"`
	Leases                []Record                `json:"leases"`
	EventDigests          []snapshotDigest        `json:"event_digests"`
	WorkUnits             []domain.WorkUnit       `json:"work_units"`
	Runs                  []domain.Run            `json:"runs"`
	RunOrder              []snapshotRunOrder      `json:"run_order"`
	RequestRuns           []snapshotRequestRun    `json:"request_runs"`
	ActiveByWorkUnit      []snapshotMembers       `json:"active_by_work_unit"`
	PendingByWorkUnit     []snapshotMembers       `json:"pending_by_work_unit"`
	WorkUnitCosts         []snapshotWorkUnitCost  `json:"work_unit_costs"`
	GovernanceIdempotency []GovernanceIdempotency `json:"governance_idempotency"`
}

// StateSnapshotChain is the Ledger's chain head as the snapshot was written:
// read after the state was copied, so it is at or past the state's own
// position, and it commits to every frame the state was built from. It is what
// binds a snapshot to one history. Two copies of a Ledger written under the
// same key can agree on every offset and sequence up to the snapshot and still
// hold different frames; their chains differ.
type StateSnapshotChain struct {
	Sequence uint64   `json:"sequence"`
	Hash     [32]byte `json:"hash"`
}

// ChainHeadReader is the part of a Log a snapshot writer reads.
type ChainHeadReader interface {
	ChainHead() (Watermark, [32]byte, bool)
}

type snapshotBalance struct {
	Key     BalanceKey `json:"key"`
	Balance Balance    `json:"balance"`
}

type snapshotReservation struct {
	AttemptID string     `json:"attempt_id"`
	Key       BalanceKey `json:"key"`
	Amount    int64      `json:"amount"`
	Lease     Event      `json:"lease"`
	Started   bool       `json:"started"`
}

type snapshotDigest struct {
	EventID string   `json:"event_id"`
	Digest  [32]byte `json:"digest"`
}

type snapshotRunOrder struct {
	WorkUnitID string   `json:"work_unit_id"`
	RunIDs     []string `json:"run_ids"`
}

type snapshotRequestRun struct {
	RequestID  string `json:"request_id"`
	WorkUnitID string `json:"work_unit_id"`
	RunID      string `json:"run_id"`
}

type snapshotMembers struct {
	WorkUnitID string   `json:"work_unit_id"`
	Members    []string `json:"members"`
}

type snapshotWorkUnitCost struct {
	WorkUnitID string       `json:"work_unit_id"`
	Cost       WorkUnitCost `json:"cost"`
}

// MarshalSnapshot encodes the state. Only the copy is made under the read
// lock; sorting and encoding happen after it is released, so appends wait
// for no more than the copy.
func (s *State) MarshalSnapshot() ([]byte, Watermark, error) {
	return s.marshalSnapshot(nil)
}

// marshalSnapshot encodes the state with the chain head log reports after the
// copy. The state only ever holds frames already committed, so a head read
// after it is at or past it and commits to every one of them.
func (s *State) marshalSnapshot(log ChainHeadReader) ([]byte, Watermark, error) {
	snapshot := s.copySnapshot()
	if log != nil {
		head, hash, ok := log.ChainHead()
		if !ok || head.Sequence < snapshot.Watermark.Sequence {
			return nil, Watermark{}, errors.New("the Ledger has no chain head covering the state")
		}
		snapshot.Chain = StateSnapshotChain{Sequence: head.Sequence, Hash: hash}
	}
	sortSnapshot(&snapshot)
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return nil, Watermark{}, fmt.Errorf("encode ledger state snapshot: %w", err)
	}
	return encoded, snapshot.Watermark, nil
}

func (s *State) copySnapshot() stateSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot := stateSnapshot{
		Version: StateSnapshotVersion, Watermark: s.watermark,
		Balances:              make([]snapshotBalance, 0, len(s.balances)),
		Reservations:          make([]snapshotReservation, 0, len(s.reservations)),
		Leases:                make([]Record, 0, len(s.leaseRecords)),
		EventDigests:          make([]snapshotDigest, 0, len(s.eventOrder)),
		WorkUnits:             make([]domain.WorkUnit, 0, len(s.workUnits)),
		Runs:                  make([]domain.Run, 0, len(s.runs)),
		RunOrder:              make([]snapshotRunOrder, 0, len(s.runsByWorkUnit)),
		RequestRuns:           make([]snapshotRequestRun, 0, len(s.requestRuns)),
		ActiveByWorkUnit:      membersOf(s.activeByWorkUnit),
		PendingByWorkUnit:     membersOf(s.pendingByWorkUnit),
		WorkUnitCosts:         make([]snapshotWorkUnitCost, 0, len(s.workUnitCosts)),
		GovernanceIdempotency: make([]GovernanceIdempotency, 0, len(s.governanceIdempotency)),
	}
	for key, balance := range s.balances {
		snapshot.Balances = append(snapshot.Balances, snapshotBalance{Key: key, Balance: balance})
	}
	for attemptID, reservation := range s.reservations {
		snapshot.Reservations = append(snapshot.Reservations, snapshotReservation{
			AttemptID: attemptID, Key: reservation.Key, Amount: reservation.Amount,
			Lease: reservation.Lease, Started: reservation.Started,
		})
	}
	for _, lease := range s.leaseRecords {
		snapshot.Leases = append(snapshot.Leases, lease)
	}
	// Oldest first, so a restored window forgets in the order this one would.
	for index := range s.eventOrder {
		eventID := s.eventOrder[(s.eventNext+index)%len(s.eventOrder)]
		snapshot.EventDigests = append(snapshot.EventDigests, snapshotDigest{EventID: eventID, Digest: s.eventDigests[eventID]})
	}
	for _, workUnit := range s.workUnits {
		snapshot.WorkUnits = append(snapshot.WorkUnits, workUnit)
	}
	for _, run := range s.runs {
		snapshot.Runs = append(snapshot.Runs, run)
	}
	for workUnitID, runIDs := range s.runsByWorkUnit {
		snapshot.RunOrder = append(snapshot.RunOrder, snapshotRunOrder{WorkUnitID: workUnitID, RunIDs: append([]string(nil), runIDs...)})
	}
	for requestID, attribution := range s.requestRuns {
		snapshot.RequestRuns = append(snapshot.RequestRuns, snapshotRequestRun{RequestID: requestID, WorkUnitID: attribution.WorkUnitID, RunID: attribution.RunID})
	}
	for workUnitID, cost := range s.workUnitCosts {
		snapshot.WorkUnitCosts = append(snapshot.WorkUnitCosts, snapshotWorkUnitCost{WorkUnitID: workUnitID, Cost: cost})
	}
	for _, idempotency := range s.governanceIdempotency {
		snapshot.GovernanceIdempotency = append(snapshot.GovernanceIdempotency, idempotency)
	}
	return snapshot
}

func membersOf(sets map[string]map[string]struct{}) []snapshotMembers {
	result := make([]snapshotMembers, 0, len(sets))
	for workUnitID, set := range sets {
		members := make([]string, 0, len(set))
		for member := range set {
			members = append(members, member)
		}
		sort.Strings(members)
		result = append(result, snapshotMembers{WorkUnitID: workUnitID, Members: members})
	}
	return result
}

func sortSnapshot(snapshot *stateSnapshot) {
	sort.Slice(snapshot.Balances, func(i, j int) bool {
		left, right := snapshot.Balances[i].Key, snapshot.Balances[j].Key
		if left.ProjectID != right.ProjectID {
			return left.ProjectID < right.ProjectID
		}
		if left.PeriodID != right.PeriodID {
			return left.PeriodID < right.PeriodID
		}
		return left.TimezoneVersion < right.TimezoneVersion
	})
	sort.Slice(snapshot.Reservations, func(i, j int) bool {
		return snapshot.Reservations[i].AttemptID < snapshot.Reservations[j].AttemptID
	})
	sort.Slice(snapshot.Leases, func(i, j int) bool { return snapshot.Leases[i].Sequence < snapshot.Leases[j].Sequence })
	sort.Slice(snapshot.WorkUnits, func(i, j int) bool { return snapshot.WorkUnits[i].ID < snapshot.WorkUnits[j].ID })
	sort.Slice(snapshot.Runs, func(i, j int) bool { return snapshot.Runs[i].ID < snapshot.Runs[j].ID })
	sort.Slice(snapshot.RunOrder, func(i, j int) bool { return snapshot.RunOrder[i].WorkUnitID < snapshot.RunOrder[j].WorkUnitID })
	sort.Slice(snapshot.RequestRuns, func(i, j int) bool {
		return snapshot.RequestRuns[i].RequestID < snapshot.RequestRuns[j].RequestID
	})
	sort.Slice(snapshot.ActiveByWorkUnit, func(i, j int) bool {
		return snapshot.ActiveByWorkUnit[i].WorkUnitID < snapshot.ActiveByWorkUnit[j].WorkUnitID
	})
	sort.Slice(snapshot.PendingByWorkUnit, func(i, j int) bool {
		return snapshot.PendingByWorkUnit[i].WorkUnitID < snapshot.PendingByWorkUnit[j].WorkUnitID
	})
	sort.Slice(snapshot.WorkUnitCosts, func(i, j int) bool {
		return snapshot.WorkUnitCosts[i].WorkUnitID < snapshot.WorkUnitCosts[j].WorkUnitID
	})
	sort.Slice(snapshot.GovernanceIdempotency, func(i, j int) bool {
		left, right := snapshot.GovernanceIdempotency[i], snapshot.GovernanceIdempotency[j]
		return governanceIdempotencyMapKey(left.ProjectID, left.Operation, left.KeyHash) <
			governanceIdempotencyMapKey(right.ProjectID, right.Operation, right.KeyHash)
	})
}

// UnmarshalStateSnapshot rebuilds a state from MarshalSnapshot's encoding.
func UnmarshalStateSnapshot(encoded []byte) (*State, error) {
	state, _, err := unmarshalStateSnapshot(encoded)
	return state, err
}

func unmarshalStateSnapshot(encoded []byte) (*State, StateSnapshotChain, error) {
	var snapshot stateSnapshot
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, StateSnapshotChain{}, fmt.Errorf("%w: %v", ErrStateSnapshotUnusable, err)
	}
	if snapshot.Version != StateSnapshotVersion {
		return nil, StateSnapshotChain{}, fmt.Errorf("%w: version %d", ErrStateSnapshotUnusable, snapshot.Version)
	}
	if len(snapshot.EventDigests) > recentEventDigests {
		return nil, StateSnapshotChain{}, fmt.Errorf("%w: %d event digests", ErrStateSnapshotUnusable, len(snapshot.EventDigests))
	}
	state := NewState()
	state.watermark = snapshot.Watermark
	for _, item := range snapshot.Balances {
		state.balances[item.Key] = item.Balance
	}
	for _, item := range snapshot.Reservations {
		state.reservations[item.AttemptID] = attemptReservation{Key: item.Key, Amount: item.Amount, Lease: item.Lease, Started: item.Started}
	}
	for _, lease := range snapshot.Leases {
		state.leaseRecords[lease.Event.AttemptID] = lease
	}
	for _, item := range snapshot.EventDigests {
		state.rememberDigest(item.EventID, item.Digest)
	}
	for _, workUnit := range snapshot.WorkUnits {
		state.workUnits[workUnit.ID] = workUnit
	}
	for _, run := range snapshot.Runs {
		state.runs[run.ID] = run
	}
	for _, item := range snapshot.RunOrder {
		state.runsByWorkUnit[item.WorkUnitID] = item.RunIDs
	}
	for _, item := range snapshot.RequestRuns {
		state.requestRuns[item.RequestID] = requestAttribution{WorkUnitID: item.WorkUnitID, RunID: item.RunID}
	}
	for _, item := range snapshot.ActiveByWorkUnit {
		state.activeByWorkUnit[item.WorkUnitID] = setOf(item.Members)
	}
	for _, item := range snapshot.PendingByWorkUnit {
		state.pendingByWorkUnit[item.WorkUnitID] = setOf(item.Members)
	}
	for _, item := range snapshot.WorkUnitCosts {
		state.workUnitCosts[item.WorkUnitID] = item.Cost
	}
	for _, item := range snapshot.GovernanceIdempotency {
		state.governanceIdempotency[governanceIdempotencyMapKey(item.ProjectID, item.Operation, item.KeyHash)] = item
	}
	return state, snapshot.Chain, nil
}

func setOf(members []string) map[string]struct{} {
	set := make(map[string]struct{}, len(members))
	for _, member := range members {
		set[member] = struct{}{}
	}
	return set
}

// WriteStateSnapshot writes the state to path under a MAC: a temporary file,
// fsynced, renamed over the old snapshot, and the directory fsynced, so a
// crash leaves either the old snapshot or the new one. It returns the position
// the snapshot describes and its size. The snapshot carries log's chain head,
// which a start checks the Ledger it resumes against.
func WriteStateSnapshot(path string, key []byte, state *State, log ChainHeadReader) (Watermark, int, error) {
	if len(key) != 32 {
		return Watermark{}, 0, errors.New("ledger state snapshot key must be 32 bytes")
	}
	if log == nil {
		return Watermark{}, 0, errors.New("ledger state snapshot needs the Ledger's chain head")
	}
	encoded, watermark, err := state.marshalSnapshot(log)
	if err != nil {
		return Watermark{}, 0, err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return Watermark{}, 0, err
	}
	temporary, err := os.CreateTemp(directory, ".ledger-state-*.tmp")
	if err != nil {
		return Watermark{}, 0, err
	}
	defer os.Remove(temporary.Name())
	_, writeErr := temporary.Write(append(encoded, stateSnapshotMAC(key, encoded)...))
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return Watermark{}, 0, fmt.Errorf("write ledger state snapshot: %w", err)
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return Watermark{}, 0, fmt.Errorf("install ledger state snapshot: %w", err)
	}
	if err := durable.SyncDirectory(directory); err != nil {
		return Watermark{}, 0, err
	}
	return watermark, len(encoded) + sha256.Size, nil
}

// ReadStateSnapshot reads and authenticates the snapshot at path. A missing
// file is reported as os.ErrNotExist; anything present that cannot be trusted
// is ErrStateSnapshotUnusable.
func ReadStateSnapshot(path string, key []byte) (*State, StateSnapshotChain, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, StateSnapshotChain{}, err
	}
	if len(raw) < sha256.Size {
		return nil, StateSnapshotChain{}, fmt.Errorf("%w: truncated", ErrStateSnapshotUnusable)
	}
	encoded, tag := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	if !hmac.Equal(tag, stateSnapshotMAC(key, encoded)) {
		return nil, StateSnapshotChain{}, fmt.Errorf("%w: authentication failed", ErrStateSnapshotUnusable)
	}
	return unmarshalStateSnapshot(encoded)
}

func stateSnapshotMAC(key, encoded []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(stateSnapshotMACLabel)
	mac.Write(encoded)
	return mac.Sum(nil)
}
