package replication

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/durable"
)

const (
	transitionJournalVersion  = 1
	maxTransitionRecordBytes  = 4096
	maxTransitionJournalBytes = 64 << 20
	transitionSeekStride      = 64
)

var transitionJournalDomain = []byte("halro:cluster:v1\x00transition-journal\x00")

var ErrInvalidTransitionCursor = errors.New("transition journal cursor does not match committed chain")

type transitionTuple struct {
	Role         Role   `json:"role"`
	Term         uint64 `json:"term"`
	PromisedTerm uint64 `json:"promised_term"`
}

func stateTransitionTuple(state MemberState) transitionTuple {
	return transitionTuple{Role: state.Role, Term: state.Term, PromisedTerm: state.PromisedTerm}
}

type transitionJournalRecord struct {
	Version          int             `json:"version"`
	JournalID        string          `json:"journal_id"`
	ClusterID        string          `json:"cluster_id"`
	Incarnation      string          `json:"incarnation"`
	NodeID           string          `json:"node_id"`
	Sequence         uint64          `json:"sequence"`
	Kind             string          `json:"kind"`
	At               time.Time       `json:"at"`
	From             transitionTuple `json:"from"`
	To               transitionTuple `json:"to"`
	PreviousDigest   string          `json:"previous_digest,omitempty"`
	BaselineStateMAC string          `json:"baseline_state_mac,omitempty"`
	MAC              string          `json:"mac"`
}

// TransitionJournal stores authenticated intents. The authenticated state
// cursor, not the presence of a journal record, commits a transition.
type TransitionJournal struct {
	path         string
	file         *os.File
	segments     []transitionSegment
	segmentLimit int64
	key          [sha256.Size]byte
	readOnly     bool
	baseline     transitionJournalRecord
	committedRec transitionJournalRecord
	pending      *transitionJournalRecord
	pendingAt    int64
	recordCount  uint64
	committed    uint64
}

// Closed segments are never deleted by the member. Their deterministic first
// sequence plus the authenticated record chain makes a missing segment fatal.
type transitionSegment struct {
	path        string
	start       uint64
	end         uint64
	checkpoints []transitionJournalCheckpoint
	bytes       int64
	sha256      [sha256.Size]byte
}

// TransitionJournalStorage describes the files retained by this member's
// authenticated transition journal. It is a capacity signal, not free disk
// space or proof that an external archive exists.
type TransitionJournalStorage struct {
	Segments uint64
	Bytes    uint64
}

func (j *TransitionJournal) storage() (TransitionJournalStorage, error) {
	if j.file == nil || len(j.segments) == 0 {
		return TransitionJournalStorage{}, errors.New("transition journal is closed")
	}
	var stats TransitionJournalStorage
	for _, segment := range j.segments {
		if segment.bytes < 0 || uint64(segment.bytes) > uint64(maxTransitionJournalBytes) {
			return TransitionJournalStorage{}, errors.New("transition journal segment has invalid capacity accounting")
		}
		stats.Bytes += uint64(segment.bytes)
		stats.Segments++
	}
	active, err := j.file.Stat()
	if err != nil || !active.Mode().IsRegular() || active.Mode().Perm() != 0o600 || active.Size() != j.segments[len(j.segments)-1].bytes {
		return TransitionJournalStorage{}, errors.New("transition journal active segment changed during capacity read")
	}
	return stats, nil
}

type transitionJournalCheckpoint struct {
	sequence uint64
	offset   int64
	prior    transitionJournalRecord
	hasPrior bool
}

func transitionSegmentPath(path string, start uint64) string {
	return fmt.Sprintf("%s.%020d", path, start)
}

func transitionSegments(path string) ([]transitionSegment, error) {
	entries, err := os.ReadDir(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return []transitionSegment{{path: path, start: 0}}, nil
	}
	if err != nil {
		return nil, err
	}
	segments := []transitionSegment{{path: path, start: 0}}
	prefix := filepath.Base(path) + "."
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		suffix := strings.TrimPrefix(entry.Name(), prefix)
		if len(suffix) != 20 {
			return nil, errors.New("transition journal has an unrecognized segment")
		}
		start, err := strconv.ParseUint(suffix, 10, 64)
		if err != nil || start == 0 || transitionSegmentPath(path, start) != filepath.Join(filepath.Dir(path), entry.Name()) {
			return nil, errors.New("transition journal has an invalid segment name")
		}
		segments = append(segments, transitionSegment{path: filepath.Join(filepath.Dir(path), entry.Name()), start: start})
	}
	closed := segments[1:]
	sort.Slice(closed, func(i, j int) bool { return closed[i].start < closed[j].start })
	return segments, nil
}

type DurableTransitionCursor struct {
	JournalID string `json:"journal_id"`
	Sequence  uint64 `json:"sequence"`
	Digest    string `json:"digest"`
}

type DurableTransitionEvidence struct {
	MemberTransitionEvent
	PreviousDigest string `json:"previous_digest"`
	Digest         string `json:"digest"`
}

type DurableTransitionPage struct {
	Version           int                         `json:"version"`
	ClusterID         string                      `json:"cluster_id"`
	Incarnation       string                      `json:"incarnation"`
	NodeID            string                      `json:"node_id"`
	BaselineKind      string                      `json:"baseline_kind"`
	BaselineDigest    string                      `json:"baseline_digest"`
	BaselineRole      Role                        `json:"baseline_role"`
	BaselineTerm      uint64                      `json:"baseline_term"`
	BaselinePromised  uint64                      `json:"baseline_promised_term"`
	CommittedSequence uint64                      `json:"committed_sequence"`
	HeadDigest        string                      `json:"head_digest"`
	Events            []DurableTransitionEvidence `json:"events"`
	NextCursor        DurableTransitionCursor     `json:"next_cursor"`
	HasMore           bool                        `json:"has_more"`
}

func publicTransitionDigest(macText string) string {
	mac, _ := parseDigestText(macText)
	digest := sha256.Sum256(mac[:])
	return hex.EncodeToString(digest[:])
}

// Page returns only state-committed transitions. A cursor is scoped to this
// journal and its prior page digest; callers cannot skip or join chains by
// supplying a sequence number alone. The returned digests are hashes of the
// private record MACs, not the record MACs themselves.
func (j *TransitionJournal) Page(after uint64, digest string, limit int) (DurableTransitionPage, error) {
	if j.file == nil || limit < 1 || limit > 64 || after > j.committed {
		return DurableTransitionPage{}, ErrInvalidTransitionCursor
	}
	if after > 0 && digest == "" {
		return DurableTransitionPage{}, ErrInvalidTransitionCursor
	}
	last := j.committed
	if last-after > uint64(limit) {
		last = after + uint64(limit)
	}
	events := make([]DurableTransitionEvidence, 0, last-after)
	var lastRecord transitionJournalRecord
	err := j.visitRecords(after, last, func(record transitionJournalRecord, prior *transitionJournalRecord) error {
		if record.Sequence == after {
			if digest != "" && digest != publicTransitionDigest(record.MAC) {
				return ErrInvalidTransitionCursor
			}
			lastRecord = record
			return nil
		}
		if prior == nil {
			return errors.New("transition journal page lost its predecessor")
		}
		events = append(events, DurableTransitionEvidence{
			MemberTransitionEvent: MemberTransitionEvent{
				Sequence: record.Sequence, At: record.At, Kind: record.Kind,
				FromRole: record.From.Role, ToRole: record.To.Role,
				FromTerm: record.From.Term, ToTerm: record.To.Term,
				FromPromisedTerm: record.From.PromisedTerm, ToPromisedTerm: record.To.PromisedTerm,
			},
			PreviousDigest: publicTransitionDigest(prior.MAC),
			Digest:         publicTransitionDigest(record.MAC),
		})
		lastRecord = record
		return nil
	})
	if err != nil {
		return DurableTransitionPage{}, err
	}
	baseline := j.baseline
	head := j.committedRec
	return DurableTransitionPage{
		Version: transitionJournalVersion, ClusterID: baseline.ClusterID, Incarnation: baseline.Incarnation,
		NodeID: baseline.NodeID, BaselineKind: baseline.Kind,
		BaselineDigest: publicTransitionDigest(baseline.MAC),
		BaselineRole:   baseline.To.Role, BaselineTerm: baseline.To.Term, BaselinePromised: baseline.To.PromisedTerm,
		CommittedSequence: head.Sequence, HeadDigest: publicTransitionDigest(head.MAC),
		Events: events, NextCursor: DurableTransitionCursor{
			JournalID: baseline.JournalID, Sequence: last, Digest: publicTransitionDigest(lastRecord.MAC),
		}, HasMore: last < j.committed,
	}, nil
}

// visitRecords reauthenticates the requested bounded range from a sparse
// checkpoint. It never seeks the append descriptor used by state publication.
func (j *TransitionJournal) visitRecords(start, end uint64, visit func(transitionJournalRecord, *transitionJournalRecord) error) error {
	var visited uint64
	for _, segment := range j.segments {
		if segment.end < start || segment.start > end {
			continue
		}
		if len(segment.checkpoints) == 0 {
			return errors.New("transition journal segment has no seek checkpoint")
		}
		first := max(start, segment.start)
		last := min(end, segment.end)
		checkpoint := segment.checkpoints[0]
		for _, candidate := range segment.checkpoints {
			if candidate.sequence > first {
				break
			}
			checkpoint = candidate
		}
		info, err := os.Lstat(segment.path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() == 0 || info.Size() > maxTransitionJournalBytes {
			return errors.New("transition journal page segment is missing or invalid")
		}
		file, err := os.Open(segment.path)
		if err != nil {
			return err
		}
		if _, err := file.Seek(checkpoint.offset, io.SeekStart); err != nil {
			_ = file.Close()
			return err
		}
		reader := bufio.NewReaderSize(file, maxTransitionRecordBytes)
		var prior *transitionJournalRecord
		if checkpoint.hasPrior {
			copyPrior := checkpoint.prior
			prior = &copyPrior
		}
		for expected := checkpoint.sequence; ; expected++ {
			line, err := reader.ReadSlice('\n')
			if err != nil {
				_ = file.Close()
				return errors.New("transition journal page record is missing or incomplete")
			}
			record, err := decodeTransitionRecord(line, j.key[:])
			if err == nil {
				err = validateJournalRecord(record, prior)
			}
			if err != nil || record.Sequence != expected {
				_ = file.Close()
				return errors.New("transition journal page record failed authentication or continuity")
			}
			if expected >= first {
				if err := visit(record, prior); err != nil {
					_ = file.Close()
					return err
				}
				visited++
			}
			prior = &record
			if expected == last {
				break
			}
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	if visited != end-start+1 {
		return errors.New("transition journal page range has a gap")
	}
	return nil
}

func transitionRecordMAC(record transitionJournalRecord, key []byte) ([sha256.Size]byte, error) {
	record.MAC = ""
	encoded, err := json.Marshal(record)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(transitionJournalDomain)
	_, _ = mac.Write(encoded)
	var digest [sha256.Size]byte
	copy(digest[:], mac.Sum(nil))
	return digest, nil
}

func encodeTransitionRecord(record transitionJournalRecord, key []byte) ([]byte, error) {
	mac, err := transitionRecordMAC(record, key)
	if err != nil {
		return nil, err
	}
	record.MAC = digestText(mac)
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if len(encoded)+1 > maxTransitionRecordBytes {
		return nil, errors.New("transition journal record exceeds size bound")
	}
	return append(encoded, '\n'), nil
}

func decodeTransitionRecord(line []byte, key []byte) (transitionJournalRecord, error) {
	if len(line) == 0 || len(line) > maxTransitionRecordBytes || line[len(line)-1] != '\n' {
		return transitionJournalRecord{}, errors.New("transition journal record is incomplete or oversized")
	}
	decoder := json.NewDecoder(bytes.NewReader(line[:len(line)-1]))
	decoder.DisallowUnknownFields()
	var record transitionJournalRecord
	if err := decoder.Decode(&record); err != nil {
		return transitionJournalRecord{}, fmt.Errorf("decode transition journal record: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return transitionJournalRecord{}, errors.New("transition journal record has trailing content")
	}
	provided, err := parseDigestText(record.MAC)
	if err != nil {
		return transitionJournalRecord{}, fmt.Errorf("transition journal MAC: %w", err)
	}
	expected, err := transitionRecordMAC(record, key)
	if err != nil {
		return transitionJournalRecord{}, err
	}
	if !hmac.Equal(provided[:], expected[:]) {
		return transitionJournalRecord{}, errors.New("transition journal MAC mismatch")
	}
	return record, nil
}

func validateTransitionTuple(tuple transitionTuple) bool {
	return (tuple.Role == RolePrimary || tuple.Role == RoleReplica || tuple.Role == RoleAwaitingDecision) &&
		tuple.PromisedTerm >= tuple.Term && (tuple.Role == RoleAwaitingDecision || tuple.Term > 0)
}

func validTransitionKind(kind string) bool {
	return kind == "promise" || kind == "promote" || kind == "adopt_higher_term"
}

func validTransitionChange(kind string, from, to transitionTuple) bool {
	switch kind {
	case "promise":
		role := from.Role
		if role == RolePrimary {
			role = RoleReplica
		}
		return to.Role == role && to.Term == from.Term && to.PromisedTerm > from.PromisedTerm
	case "promote":
		return from.Role == RoleReplica && to.Role == RolePrimary && to.Term == from.PromisedTerm &&
			to.Term > from.Term && to.PromisedTerm == from.PromisedTerm
	case "adopt_higher_term":
		return to.Role == RoleReplica && to.Term >= from.Term && to.PromisedTerm >= to.Term &&
			to.PromisedTerm >= from.PromisedTerm && to != from
	default:
		return false
	}
}

func validateJournalRecord(record transitionJournalRecord, prior *transitionJournalRecord) error {
	if record.Version != transitionJournalVersion || len(record.JournalID) != 32 ||
		strings.Trim(record.JournalID, "0123456789abcdef") != "" || record.ClusterID == "" ||
		record.Incarnation == "" || record.NodeID == "" || record.At.IsZero() || !validateTransitionTuple(record.To) ||
		!validateTransitionTuple(record.From) {
		return errors.New("transition journal record has invalid identity or state")
	}
	if prior == nil {
		if record.Sequence != 0 || record.PreviousDigest != "" || record.From != record.To || record.BaselineStateMAC == "" ||
			(record.Kind != "legacy_baseline" && record.Kind != "initial_state") {
			return errors.New("transition journal baseline is invalid")
		}
		if _, err := parseDigestText(record.BaselineStateMAC); err != nil {
			return errors.New("transition journal baseline state MAC is invalid")
		}
		return nil
	}
	if record.JournalID != prior.JournalID || record.ClusterID != prior.ClusterID ||
		record.Incarnation != prior.Incarnation || record.NodeID != prior.NodeID ||
		record.Sequence != prior.Sequence+1 || record.PreviousDigest != prior.MAC ||
		record.From != prior.To || !validTransitionKind(record.Kind) ||
		!validTransitionChange(record.Kind, record.From, record.To) ||
		record.At.Before(prior.At) || record.BaselineStateMAC != "" {
		return errors.New("transition journal identity, sequence or tuple chain is invalid")
	}
	return nil
}

// CreateTransitionJournal writes a new authenticated baseline before a caller
// publishes version-3 state. The caller still owns the member data lock and
// must not treat the returned cursor as committed until WriteState succeeds.
func CreateTransitionJournal(path string, key []byte, state MemberState, baselineKind string) (TransitionCursor, error) {
	if len(key) != sha256.Size || (baselineKind != "legacy_baseline" && baselineKind != "initial_state") {
		return TransitionCursor{}, errors.New("transition journal requires a key and fixed baseline kind")
	}
	if err := state.Validate(); err != nil {
		return TransitionCursor{}, err
	}
	if state.Version != StateVersion {
		return TransitionCursor{}, errors.New("transition journal baseline requires version-2 member state")
	}
	if _, err := os.Lstat(path); err == nil {
		return TransitionCursor{}, errors.New("transition journal already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return TransitionCursor{}, err
	}
	segments, err := transitionSegments(path)
	if err != nil {
		return TransitionCursor{}, err
	}
	if len(segments) != 1 {
		return TransitionCursor{}, errors.New("transition journal segment exists without its baseline")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return TransitionCursor{}, err
	}
	stateMAC, err := memberStateMAC(state, key)
	if err != nil {
		return TransitionCursor{}, err
	}
	record := transitionJournalRecord{
		Version: transitionJournalVersion, JournalID: hex.EncodeToString(id[:]), ClusterID: state.ClusterID,
		Incarnation: state.Incarnation, NodeID: state.NodeID, Kind: baselineKind, At: time.Now().UTC(),
		From: stateTransitionTuple(state), To: stateTransitionTuple(state),
		BaselineStateMAC: digestText(stateMAC),
	}
	encoded, err := encodeTransitionRecord(record, key)
	if err != nil {
		return TransitionCursor{}, err
	}
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".transition-journal-*")
	if err != nil {
		return TransitionCursor{}, err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return TransitionCursor{}, err
	}
	if err := writeAll(file, encoded); err != nil {
		_ = file.Close()
		return TransitionCursor{}, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return TransitionCursor{}, err
	}
	if err := file.Close(); err != nil {
		return TransitionCursor{}, err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return TransitionCursor{}, err
	}
	if err := durable.SyncDirectory(directory); err != nil {
		return TransitionCursor{}, err
	}
	mac, err := transitionRecordMAC(record, key)
	if err != nil {
		return TransitionCursor{}, err
	}
	return TransitionCursor{JournalID: record.JournalID, Sequence: 0, Digest: mac}, nil
}

// OpenTransitionJournal verifies the full chain and the exact committed state
// cursor. One final uncommitted intent may be present after a crash.
func OpenTransitionJournal(path string, key []byte, state MemberState) (*TransitionJournal, error) {
	return openTransitionJournal(path, key, state, false)
}

func openTransitionJournal(path string, key []byte, state MemberState, readOnly bool) (*TransitionJournal, error) {
	if len(key) != sha256.Size || (state.Version != TransitionStateVersion && state.Version != StateVersion) {
		return nil, errors.New("transition journal requires an authenticated member state and 32-byte key")
	}
	if err := state.Validate(); err != nil {
		return nil, err
	}
	segments, err := transitionSegments(path)
	if err != nil {
		return nil, fmt.Errorf("inspect transition journal segments: %w", err)
	}
	var file *os.File
	fail := func(openErr error) (*TransitionJournal, error) {
		if file != nil {
			_ = file.Close()
		}
		return nil, openErr
	}
	var baseline, previous, committedRec transitionJournalRecord
	var pending *transitionJournalRecord
	var pendingAt int64
	var recordCount uint64
	for segmentIndex := range segments {
		segment := &segments[segmentIndex]
		info, err := os.Lstat(segment.path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() == 0 || info.Size() > maxTransitionJournalBytes {
			return fail(errors.New("transition journal segment must be a nonempty private regular file within its size bound"))
		}
		if segmentIndex == len(segments)-1 && !readOnly {
			file, err = os.OpenFile(segment.path, os.O_RDWR|os.O_APPEND, 0)
		} else {
			file, err = os.Open(segment.path)
		}
		if err != nil {
			return fail(err)
		}
		reader := bufio.NewReaderSize(file, maxTransitionRecordBytes)
		var checksum hash.Hash
		if readOnly {
			checksum = sha256.New()
		}
		var size int64
		first := true
		for {
			line, readErr := reader.ReadSlice('\n')
			if readErr == io.EOF && len(line) == 0 {
				break
			}
			if readErr != nil {
				return fail(errors.New("transition journal record is incomplete or oversized"))
			}
			offset := size
			size += int64(len(line))
			if checksum != nil {
				_, _ = checksum.Write(line)
			}
			if size > maxTransitionJournalBytes {
				return fail(errors.New("transition journal segment exceeds size bound"))
			}
			record, err := decodeTransitionRecord(line, key)
			if err != nil {
				return fail(err)
			}
			if first && record.Sequence != segment.start {
				return fail(errors.New("transition journal segment starts at the wrong sequence"))
			}
			first = false
			var prior *transitionJournalRecord
			if recordCount > 0 {
				prior = &previous
			}
			if err := validateJournalRecord(record, prior); err != nil {
				return fail(err)
			}
			if (record.Sequence-segment.start)%transitionSeekStride == 0 {
				checkpoint := transitionJournalCheckpoint{sequence: record.Sequence, offset: offset}
				if prior != nil {
					checkpoint.prior, checkpoint.hasPrior = *prior, true
				}
				segment.checkpoints = append(segment.checkpoints, checkpoint)
			}
			if recordCount == 0 {
				baseline = record
			}
			if state.Version == TransitionStateVersion {
				if record.Sequence == state.Transition.Sequence {
					committedRec = record
				} else if record.Sequence > state.Transition.Sequence && record.Sequence-state.Transition.Sequence == 1 {
					copyPending := record
					pending, pendingAt = &copyPending, offset
				}
			}
			previous = record
			segment.end = record.Sequence
			recordCount++
		}
		if first {
			return fail(errors.New("transition journal segment has no records"))
		}
		if size != info.Size() {
			return fail(errors.New("transition journal segment changed during verification"))
		}
		segment.bytes = size
		if checksum != nil {
			copy(segment.sha256[:], checksum.Sum(nil))
		}
		if segmentIndex < len(segments)-1 {
			if err := file.Close(); err != nil {
				file = nil
				return nil, err
			}
			file = nil
		}
	}
	if recordCount == 0 || state.Version == TransitionStateVersion && baseline.JournalID != state.Transition.JournalID ||
		baseline.ClusterID != state.ClusterID || baseline.Incarnation != state.Incarnation || baseline.NodeID != state.NodeID {
		return fail(errors.New("transition journal does not match member state identity"))
	}
	var committed uint64
	if state.Version == StateVersion {
		stateMAC, err := memberStateMAC(state, key)
		if err != nil {
			return fail(err)
		}
		if recordCount != 1 || baseline.Kind != "legacy_baseline" || baseline.To != stateTransitionTuple(state) ||
			baseline.BaselineStateMAC != digestText(stateMAC) {
			return fail(errors.New("version-2 migration journal is not a matching baseline"))
		}
		committedRec = baseline
	} else {
		if state.Transition.Sequence >= recordCount || recordCount-state.Transition.Sequence > 2 {
			return fail(errors.New("transition journal committed cursor or pending suffix is invalid"))
		}
		committed = state.Transition.Sequence
		committedDigest, err := parseDigestText(committedRec.MAC)
		if err != nil || committedDigest != state.Transition.Digest || committedRec.To != stateTransitionTuple(state) {
			return fail(errors.New("transition journal does not match authenticated state cursor"))
		}
	}
	journal := &TransitionJournal{path: path, file: file, segments: segments, segmentLimit: maxTransitionJournalBytes,
		baseline: baseline, committedRec: committedRec, pending: pending, pendingAt: pendingAt,
		recordCount: recordCount, committed: committed, readOnly: readOnly}
	copy(journal.key[:], key)
	return journal, nil
}

// MigrateStateTransitionJournal performs the state/journal publication half
// of an explicit offline migration. The caller must hold the member data lock
// and ensure old binaries cannot start. Repeating it after a crash verifies
// and reuses the same authenticated baseline rather than minting a new chain.
func MigrateStateTransitionJournal(statePath string, key []byte) (MemberState, error) {
	state, err := ReadState(statePath, key)
	if err != nil {
		return MemberState{}, err
	}
	journalPath := filepath.Join(filepath.Dir(statePath), "transitions.journal")
	if state.Version == TransitionStateVersion {
		journal, err := OpenTransitionJournal(journalPath, key, state)
		if err != nil {
			return MemberState{}, err
		}
		defer journal.Close()
		return state, nil
	}
	if state.Version != StateVersion {
		return MemberState{}, errors.New("transition migration requires version-2 member state")
	}
	if _, err := os.Lstat(journalPath); errors.Is(err, os.ErrNotExist) {
		if _, err := CreateTransitionJournal(journalPath, key, state, "legacy_baseline"); err != nil {
			return MemberState{}, err
		}
	} else if err != nil {
		return MemberState{}, err
	}
	journal, err := OpenTransitionJournal(journalPath, key, state)
	if err != nil {
		return MemberState{}, err
	}
	defer journal.Close()
	state.Version = TransitionStateVersion
	state.Transition = journal.Cursor()
	if err := WriteState(statePath, state, key); err != nil {
		return MemberState{}, err
	}
	return state, nil
}

func (j *TransitionJournal) Cursor() TransitionCursor {
	record := j.committedRec
	digest, _ := parseDigestText(record.MAC)
	return TransitionCursor{JournalID: record.JournalID, Sequence: record.Sequence, Digest: digest}
}

func (j *TransitionJournal) CommittedEvents() ([]MemberTransitionEvent, error) {
	var events []MemberTransitionEvent
	var after uint64
	var digest string
	for {
		page, err := j.Page(after, digest, 64)
		if err != nil {
			return nil, err
		}
		for _, event := range page.Events {
			events = append(events, event.MemberTransitionEvent)
		}
		if !page.HasMore {
			return events, nil
		}
		after, digest = page.NextCursor.Sequence, page.NextCursor.Digest
	}
}

// ResolvePending discards one authenticated but uncommitted final intent only
// after OpenTransitionJournal has matched the committed state cursor. An atomic
// replacement makes repeated recovery safe across another crash.
func (j *TransitionJournal) ResolvePending() error {
	if j.readOnly {
		return errors.New("read-only transition snapshot cannot resolve pending intent")
	}
	if j.pending == nil {
		return nil
	}
	if j.file == nil || j.pending.Sequence != j.committed+1 || j.recordCount != j.committed+2 {
		return errors.New("transition journal pending suffix is invalid")
	}
	directory := filepath.Dir(j.path)
	active := &j.segments[len(j.segments)-1]
	if active.start == j.committed+1 {
		if len(j.segments) < 2 {
			return errors.New("transition journal baseline cannot be discarded")
		}
		if err := j.file.Close(); err != nil {
			j.file = nil
			return err
		}
		j.file = nil
		if err := os.Remove(active.path); err != nil {
			return err
		}
		if err := durable.SyncDirectory(directory); err != nil {
			return err
		}
		j.segments = j.segments[:len(j.segments)-1]
		file, err := os.OpenFile(j.segments[len(j.segments)-1].path, os.O_RDWR|os.O_APPEND, 0)
		if err != nil {
			return err
		}
		j.file = file
		j.pending, j.pendingAt = nil, 0
		j.recordCount = j.committed + 1
		return nil
	}
	temporary, err := os.CreateTemp(directory, ".transition-recovery-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if j.pendingAt <= 0 {
		_ = temporary.Close()
		return errors.New("transition journal pending offset is invalid")
	}
	if _, err := io.CopyN(temporary, io.NewSectionReader(j.file, 0, j.pendingAt), j.pendingAt); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), active.path); err != nil {
		return err
	}
	if err := durable.SyncDirectory(directory); err != nil {
		return err
	}
	if err := j.file.Close(); err != nil {
		j.file = nil
		return err
	}
	j.file, err = os.OpenFile(active.path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	active.end = j.committed
	active.bytes = j.pendingAt
	for len(active.checkpoints) > 0 && active.checkpoints[len(active.checkpoints)-1].sequence > j.committed {
		active.checkpoints = active.checkpoints[:len(active.checkpoints)-1]
	}
	j.pending, j.pendingAt = nil, 0
	j.recordCount = j.committed + 1
	return nil
}

func (j *TransitionJournal) appendIntent(kind string, next MemberState) (TransitionCursor, error) {
	if j.readOnly || !validTransitionKind(kind) || j.file == nil || j.pending != nil || j.recordCount != j.committed+1 {
		return TransitionCursor{}, errors.New("transition journal has an unresolved intent or is closed")
	}
	previous := j.committedRec
	record := transitionJournalRecord{
		Version: transitionJournalVersion, JournalID: previous.JournalID, ClusterID: previous.ClusterID,
		Incarnation: previous.Incarnation, NodeID: previous.NodeID, Sequence: previous.Sequence + 1,
		Kind: kind, At: time.Now().UTC(), From: previous.To, To: stateTransitionTuple(next), PreviousDigest: previous.MAC,
	}
	if err := validateJournalRecord(record, &previous); err != nil {
		return TransitionCursor{}, err
	}
	encoded, err := encodeTransitionRecord(record, j.key[:])
	if err != nil {
		return TransitionCursor{}, err
	}
	info, err := j.file.Stat()
	if err != nil {
		return TransitionCursor{}, err
	}
	limit := j.segmentLimit
	if limit == 0 {
		limit = maxTransitionJournalBytes
	}
	if int64(len(encoded)) > limit {
		return TransitionCursor{}, errors.New("transition journal record exceeds segment size bound")
	}
	if info.Size()+int64(len(encoded)) > limit {
		segment := transitionSegment{path: transitionSegmentPath(j.path, record.Sequence), start: record.Sequence, end: record.Sequence, bytes: int64(len(encoded)),
			checkpoints: []transitionJournalCheckpoint{{sequence: record.Sequence, offset: 0, prior: previous, hasPrior: true}}}
		newFile, err := os.OpenFile(segment.path, os.O_CREATE|os.O_EXCL|os.O_RDWR|os.O_APPEND, 0o600)
		if err != nil {
			return TransitionCursor{}, fmt.Errorf("create transition journal segment: %w", err)
		}
		if err := writeAll(newFile, encoded); err != nil {
			_ = newFile.Close()
			return TransitionCursor{}, err
		}
		if err := newFile.Sync(); err != nil {
			_ = newFile.Close()
			return TransitionCursor{}, err
		}
		if err := durable.SyncDirectory(filepath.Dir(j.path)); err != nil {
			_ = newFile.Close()
			return TransitionCursor{}, err
		}
		oldFile := j.file
		j.file = newFile
		j.segments = append(j.segments, segment)
		if err := oldFile.Close(); err != nil {
			return TransitionCursor{}, err
		}
	} else {
		if err := writeAll(j.file, encoded); err != nil {
			return TransitionCursor{}, err
		}
		if err := j.file.Sync(); err != nil {
			return TransitionCursor{}, err
		}
		active := &j.segments[len(j.segments)-1]
		if (record.Sequence-active.start)%transitionSeekStride == 0 {
			active.checkpoints = append(active.checkpoints, transitionJournalCheckpoint{
				sequence: record.Sequence, offset: info.Size(), prior: previous, hasPrior: true})
		}
		active.end = record.Sequence
		active.bytes = info.Size() + int64(len(encoded))
	}
	digest, _ := transitionRecordMAC(record, j.key[:])
	record.MAC = digestText(digest)
	j.pending = &record
	if j.segments[len(j.segments)-1].start == record.Sequence {
		j.pendingAt = 0
	} else {
		j.pendingAt = info.Size()
	}
	j.recordCount++
	return TransitionCursor{JournalID: record.JournalID, Sequence: record.Sequence, Digest: digest}, nil
}

func (j *TransitionJournal) commit(cursor TransitionCursor) error {
	if j.pending == nil || j.pending.Sequence != j.committed+1 || j.pending.Sequence != cursor.Sequence || j.Cursor().JournalID != cursor.JournalID {
		return errors.New("transition journal commit does not match pending intent")
	}
	digest, _ := parseDigestText(j.pending.MAC)
	if digest != cursor.Digest {
		return errors.New("transition journal commit digest mismatch")
	}
	j.committedRec = *j.pending
	j.pending, j.pendingAt = nil, 0
	j.committed++
	return nil
}

func (j *TransitionJournal) Close() error {
	clear(j.key[:])
	if j.file == nil {
		return nil
	}
	err := j.file.Close()
	j.file = nil
	return err
}
