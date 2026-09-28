package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	durableArchiveLimit  = 4 << 20
	durableSegmentTarget = 3 << 20
)

// Every closed generation is immutable and retained. The current manifest
// holds only events since the last rollover; its cursor still covers the full
// verified prefix from the member baseline.
type durableStoredChain struct {
	NodeID            string                   `json:"node_id"`
	Incarnation       string                   `json:"incarnation"`
	BaselineKind      string                   `json:"baseline_kind"`
	BaselineRole      string                   `json:"baseline_role"`
	BaselineTerm      uint64                   `json:"baseline_term"`
	BaselinePromised  uint64                   `json:"baseline_promised_term"`
	StartCursor       durableTransitionCursor  `json:"start_cursor"`
	StartEventAt      time.Time                `json:"start_event_at,omitempty"`
	Cursor            durableTransitionCursor  `json:"cursor"`
	LastEventAt       time.Time                `json:"last_event_at,omitempty"`
	CommittedSequence uint64                   `json:"committed_sequence"`
	HeadDigest        string                   `json:"head_digest"`
	ObservedAt        time.Time                `json:"observed_at"`
	Events            []durableTransitionEvent `json:"events"`
}

type durableArchiveDocument struct {
	Version     int                             `json:"version"`
	Generation  uint64                          `json:"generation"`
	Environment string                          `json:"environment"`
	Cluster     string                          `json:"cluster"`
	Members     []string                        `json:"members"`
	Chains      map[string][]durableStoredChain `json:"chains"`
	Handoffs    []durableReseedHandoff          `json:"handoffs,omitempty"`
}

// A handoff is an explicit assertion about two complete, separately
// authenticated member generations. Frozen source bytes and the preflight
// report must be retained independently; this collector record alone is not
// an immutable receipt or proof of fencing.
type durableReseedHandoff struct {
	NodeID                   string    `json:"node_id"`
	Incarnation              string    `json:"incarnation"`
	OldJournalID             string    `json:"old_journal_id"`
	OldCommittedSequence     uint64    `json:"old_committed_sequence"`
	OldCommittedDigest       string    `json:"old_committed_digest"`
	OldEventsSHA256          string    `json:"old_events_sha256"`
	OldMemberInventorySHA256 string    `json:"old_member_inventory_sha256"`
	NewJournalID             string    `json:"new_journal_id"`
	NewBaselineDigest        string    `json:"new_baseline_digest"`
	NewMemberInventorySHA256 string    `json:"new_member_inventory_sha256"`
	SourceNode               string    `json:"source_node"`
	SourceInventorySHA256    string    `json:"source_inventory_sha256"`
	SeedManifestSHA256       string    `json:"seed_manifest_sha256"`
	SeedTerm                 uint64    `json:"seed_term"`
	SeedIndex                uint64    `json:"seed_index"`
	FencingEvidenceSHA256    string    `json:"fencing_evidence_sha256"`
	RetiredFrozenAt          time.Time `json:"retired_frozen_at"`
	SourceFrozenAt           time.Time `json:"source_frozen_at"`
	ReplacementFrozenAt      time.Time `json:"replacement_frozen_at"`
	CommittedAt              time.Time `json:"committed_at"`
}

type durableArchiveMemberView struct {
	NodeID         string           `json:"node_id"`
	Status         string           `json:"status"`
	Incarnation    string           `json:"incarnation,omitempty"`
	JournalID      string           `json:"journal_id,omitempty"`
	BaselineKind   string           `json:"baseline_kind,omitempty"`
	StoredSequence uint64           `json:"stored_sequence"`
	ObservedHead   uint64           `json:"observed_head"`
	EventsRetained uint64           `json:"events_retained"`
	ObservedAt     time.Time        `json:"observed_at,omitempty"`
	Failure        string           `json:"failure,omitempty"`
	RecentEvents   []liveTransition `json:"recent_events,omitempty"`
}

type durableArchiveView struct {
	Status         string                     `json:"status"`
	ClosedSegments uint64                     `json:"closed_segments"`
	ManifestBytes  uint64                     `json:"manifest_bytes"`
	StorageBytes   uint64                     `json:"storage_bytes"`
	Members        []durableArchiveMemberView `json:"members,omitempty"`
}

type durableArchive struct {
	mu            sync.Mutex
	path          string
	lock          *os.File
	doc           durableArchiveDocument
	recent        map[string][]liveTransition
	segmentTarget int
	closedBytes   uint64
	manifestBytes uint64
	problems      map[string]string
	lastErr       string
}

func openDurableArchive(path, environment, cluster string, members []string) (*durableArchive, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("durable-transition-journal must be a clean absolute path")
	}
	copyMembers := append([]string(nil), members...)
	slices.Sort(copyMembers)
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("durable-transition-journal parent must be a directory without group/world write access")
	}
	lockPath := path + ".lock"
	if info, err := os.Lstat(lockPath); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return nil, errors.New("durable-transition-journal lock must be a private regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect durable-transition-journal lock: %w", err)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open durable-transition-journal lock: %w", err)
	}
	owned := false
	defer func() {
		if !owned {
			_ = lock.Close()
		}
	}()
	info, err := lock.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("durable-transition-journal lock must be a private regular file")
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.New("durable-transition-journal is already owned")
	}
	doc := durableArchiveDocument{Version: 2, Environment: environment, Cluster: cluster, Members: copyMembers, Chains: map[string][]durableStoredChain{}}
	if _, err := os.Lstat(path); err == nil {
		doc, err = readDurableArchiveDocument(path)
		if err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect durable-transition-journal: %w", err)
	}
	if doc.Environment != environment || doc.Cluster != cluster || !slices.Equal(doc.Members, copyMembers) || doc.Chains == nil {
		return nil, errors.New("durable-transition-journal identity or version mismatch")
	}
	generations, err := durableArchiveSegments(path)
	if err != nil {
		return nil, err
	}
	if doc.Version == 1 {
		if len(generations) != 0 {
			return nil, errors.New("legacy durable-transition-journal has unexpected segments")
		}
		doc.Version = 2
		for node, chains := range doc.Chains {
			for i := range chains {
				chains[i].StartCursor = durableTransitionCursor{JournalID: chains[i].Cursor.JournalID,
					BaselineDigest: chains[i].Cursor.BaselineDigest, Digest: chains[i].Cursor.BaselineDigest,
					Role: chains[i].BaselineRole, Term: chains[i].BaselineTerm, PromisedTerm: chains[i].BaselinePromised}
				if len(chains[i].Events) > 0 {
					chains[i].LastEventAt = chains[i].Events[len(chains[i].Events)-1].At
				}
			}
			doc.Chains[node] = chains
		}
		if _, _, err := replayDurableArchive(path, doc, environment, cluster, copyMembers); err != nil {
			return nil, err
		}
		if encoded, err := json.Marshal(doc); err != nil || len(encoded)+1 > durableArchiveLimit {
			return nil, errors.New("legacy durable-transition-journal cannot fit upgraded manifest")
		}
		if err := persistArchive(path, doc); err != nil {
			return nil, err
		}
	}
	if uint64(len(generations)) == doc.Generation+1 && len(generations) > 0 {
		orphan, err := readDurableArchiveDocument(durableArchiveSegmentPath(path, doc.Generation))
		if err != nil || !reflect.DeepEqual(orphan, doc) {
			return nil, errors.New("durable-transition-journal has an inconsistent unfinished rollover")
		}
		if _, _, err := replayDurableArchive(path, doc, environment, cluster, copyMembers); err != nil {
			return nil, err
		}
		next := rolledDurableArchiveDocument(doc)
		if err := persistArchive(path, next); err != nil {
			return nil, err
		}
		doc = next
	}
	if uint64(len(generations)) != doc.Generation {
		return nil, errors.New("durable-transition-journal has missing or extra segments")
	}
	recent, closedBytes, err := replayDurableArchive(path, doc, environment, cluster, copyMembers)
	if err != nil {
		return nil, err
	}
	var manifestBytes uint64
	if info, err := os.Stat(path); err == nil {
		manifestBytes = uint64(info.Size())
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	owned = true
	problems := make(map[string]string, len(copyMembers))
	for _, node := range copyMembers {
		problems[node] = "not_polled_since_restart"
	}
	return &durableArchive{path: path, lock: lock, doc: doc, recent: recent, segmentTarget: durableSegmentTarget,
		closedBytes: closedBytes, manifestBytes: manifestBytes, problems: problems}, nil
}

func validStoredChain(chain durableStoredChain) bool {
	if chain.Incarnation == "" || (chain.BaselineKind != "legacy_baseline" && chain.BaselineKind != "initial_state") ||
		!validDurableJournalID(chain.Cursor.JournalID) || !validDurableDigest(chain.Cursor.BaselineDigest) ||
		!validDurableDigest(chain.Cursor.Digest) || !validDurableDigest(chain.HeadDigest) ||
		!validDurableRole(chain.BaselineRole) || chain.BaselinePromised < chain.BaselineTerm ||
		chain.StartCursor.JournalID != chain.Cursor.JournalID || chain.StartCursor.BaselineDigest != chain.Cursor.BaselineDigest ||
		chain.StartCursor.Sequence > chain.Cursor.Sequence || chain.Cursor.Sequence-chain.StartCursor.Sequence != uint64(len(chain.Events)) ||
		chain.Cursor.Sequence > chain.CommittedSequence || chain.ObservedAt.IsZero() {
		return false
	}
	baseline := durableTransitionCursor{JournalID: chain.Cursor.JournalID, BaselineDigest: chain.Cursor.BaselineDigest,
		Digest: chain.Cursor.BaselineDigest, Role: chain.BaselineRole, Term: chain.BaselineTerm, PromisedTerm: chain.BaselinePromised}
	if chain.StartCursor.Sequence == 0 && (chain.StartCursor != baseline || !chain.StartEventAt.IsZero()) {
		return false
	}
	last := chain.StartCursor
	previousAt := chain.StartEventAt
	for _, event := range chain.Events {
		if event.Sequence != last.Sequence+1 || event.PreviousDigest != last.Digest || !validDurableDigest(event.Digest) ||
			!validDurableChange(event.liveTransition) || event.FromRole != last.Role || event.FromTerm != last.Term ||
			event.FromPromisedTerm != last.PromisedTerm || event.At.IsZero() || event.At.Before(previousAt) {
			return false
		}
		last.Sequence, last.Digest = event.Sequence, event.Digest
		last.Role, last.Term, last.PromisedTerm = event.ToRole, event.ToTerm, event.ToPromisedTerm
		previousAt = event.At
	}
	return last == chain.Cursor && chain.LastEventAt.Equal(previousAt) &&
		(chain.Cursor.Sequence != chain.CommittedSequence || chain.Cursor.Digest == chain.HeadDigest)
}

func durableArchiveSegmentPath(path string, generation uint64) string {
	return fmt.Sprintf("%s.segment.%020d", path, generation)
}

func durableArchiveSegments(path string) ([]uint64, error) {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	prefix := filepath.Base(path) + ".segment."
	var generations []uint64
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		suffix := strings.TrimPrefix(entry.Name(), prefix)
		generation, err := strconv.ParseUint(suffix, 10, 64)
		if err != nil || len(suffix) != 20 || filepath.Base(durableArchiveSegmentPath(path, generation)) != entry.Name() {
			return nil, errors.New("durable-transition-journal has an invalid segment name")
		}
		generations = append(generations, generation)
	}
	sort.Slice(generations, func(i, j int) bool { return generations[i] < generations[j] })
	for i, generation := range generations {
		if generation != uint64(i) {
			return nil, errors.New("durable-transition-journal has a missing or duplicate segment")
		}
	}
	return generations, nil
}

func readDurableArchiveDocument(path string) (durableArchiveDocument, error) {
	doc, _, err := readDurableArchivePayload(path)
	return doc, err
}

func readDurableArchivePayload(path string) (durableArchiveDocument, []byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() == 0 || info.Size() > durableArchiveLimit {
		return durableArchiveDocument{}, nil, errors.New("durable-transition-journal document must be a nonempty private regular file at most 4 MiB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return durableArchiveDocument{}, nil, err
	}
	if len(data) == 0 || len(data) > durableArchiveLimit || int64(len(data)) != info.Size() {
		return durableArchiveDocument{}, nil, errors.New("durable-transition-journal document changed while being read")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var doc durableArchiveDocument
	if err := decoder.Decode(&doc); err != nil {
		return durableArchiveDocument{}, nil, fmt.Errorf("decode durable-transition-journal document: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return durableArchiveDocument{}, nil, errors.New("durable-transition-journal document has trailing data")
	}
	return doc, data, nil
}

func rolledDurableArchiveDocument(doc durableArchiveDocument) durableArchiveDocument {
	next := doc
	next.Generation++
	next.Chains = make(map[string][]durableStoredChain, len(doc.Chains))
	for node, chains := range doc.Chains {
		copyChains := append([]durableStoredChain(nil), chains...)
		for i := range copyChains {
			copyChains[i].StartCursor = copyChains[i].Cursor
			copyChains[i].StartEventAt = copyChains[i].LastEventAt
			copyChains[i].Events = nil
		}
		next.Chains[node] = copyChains
	}
	return next
}

type durableArchiveVerifier struct {
	environment, cluster string
	members              []string
	nextGeneration       uint64
	prior                map[string]durableStoredChain
	orders               map[string][]string
	handoffs             []durableReseedHandoff
	versionThreeSeen     bool
	eventRoots           map[string]hash.Hash
	recent               map[string][]liveTransition
}

func newDurableArchiveVerifier(environment, cluster string, members []string) *durableArchiveVerifier {
	return &durableArchiveVerifier{environment: environment, cluster: cluster, members: members,
		prior: map[string]durableStoredChain{}, orders: map[string][]string{}, recent: map[string][]liveTransition{},
		eventRoots: map[string]hash.Hash{}}
}

func (v *durableArchiveVerifier) verify(doc durableArchiveDocument) error {
	if (doc.Version != 2 && doc.Version != 3) || v.versionThreeSeen && doc.Version != 3 ||
		doc.Generation != v.nextGeneration || doc.Environment != v.environment || doc.Cluster != v.cluster ||
		!slices.Equal(doc.Members, v.members) || doc.Chains == nil {
		return errors.New("durable-transition-journal identity or generation mismatch")
	}
	if doc.Version == 2 && len(doc.Handoffs) != 0 || len(doc.Handoffs) < len(v.handoffs) ||
		!slices.Equal(doc.Handoffs[:len(v.handoffs)], v.handoffs) {
		return errors.New("durable-transition-journal handoff inventory changed")
	}
	for _, handoff := range doc.Handoffs[len(v.handoffs):] {
		if !validDurableReseedHandoff(handoff, doc) {
			return errors.New("durable-transition-journal has an invalid member generation handoff")
		}
	}
	seen := map[string]bool{}
	usedHandoffs := make(map[string]bool, len(doc.Handoffs))
	for node, chains := range doc.Chains {
		if !slices.Contains(v.members, node) || len(chains) < len(v.orders[node]) {
			return errors.New("durable-transition-journal member or incarnation inventory changed")
		}
		incarnations := map[string]bool{}
		for i, chain := range chains {
			if chain.NodeID != node || !validStoredChain(chain) || seen[chain.Cursor.JournalID] {
				return errors.New("durable-transition-journal chain is inconsistent")
			}
			seen[chain.Cursor.JournalID] = true
			if incarnations[chain.Incarnation] {
				if i == 0 || chains[i-1].Incarnation != chain.Incarnation {
					return errors.New("durable-transition-journal member generation is out of order")
				}
				matched := false
				for _, handoff := range doc.Handoffs {
					if handoff.NodeID == node && handoff.Incarnation == chain.Incarnation &&
						handoff.OldJournalID == chains[i-1].Cursor.JournalID && handoff.NewJournalID == chain.Cursor.JournalID &&
						handoff.NewBaselineDigest == chain.Cursor.BaselineDigest &&
						chain.BaselineRole == "replica" && chain.BaselineTerm == handoff.SeedTerm &&
						chain.BaselinePromised == handoff.SeedTerm &&
						chains[i-1].Cursor.Sequence == handoff.OldCommittedSequence &&
						chains[i-1].Cursor.Digest == handoff.OldCommittedDigest &&
						chains[i-1].CommittedSequence == handoff.OldCommittedSequence &&
						chains[i-1].HeadDigest == handoff.OldCommittedDigest {
						matched = true
						usedHandoffs[handoff.NewJournalID] = true
						break
					}
				}
				if !matched {
					return errors.New("durable-transition-journal member generation lacks a matching handoff")
				}
			}
			incarnations[chain.Incarnation] = true
			if i < len(v.orders[node]) {
				if v.orders[node][i] != chain.Incarnation+":"+chain.Cursor.JournalID {
					return errors.New("durable-transition-journal incarnation order changed")
				}
			} else {
				v.orders[node] = append(v.orders[node], chain.Incarnation+":"+chain.Cursor.JournalID)
			}
			if previous, exists := v.prior[chain.Cursor.JournalID]; exists {
				if chain.StartCursor != previous.Cursor || !chain.StartEventAt.Equal(previous.LastEventAt) ||
					chain.NodeID != previous.NodeID ||
					chain.Incarnation != previous.Incarnation || chain.BaselineKind != previous.BaselineKind ||
					chain.BaselineRole != previous.BaselineRole || chain.BaselineTerm != previous.BaselineTerm ||
					chain.BaselinePromised != previous.BaselinePromised || chain.CommittedSequence < previous.CommittedSequence ||
					chain.ObservedAt.Before(previous.ObservedAt) {
					return errors.New("durable-transition-journal cross-segment chain mismatch")
				}
			} else if chain.StartCursor.Sequence != 0 {
				return errors.New("durable-transition-journal chain is missing its baseline")
			}
			chain.Events = nil // keep only the cursor and metadata, never all historical page payloads
			v.prior[chain.Cursor.JournalID] = chain
			root := v.eventRoots[chain.Cursor.JournalID]
			if root == nil {
				root = sha256.New()
				v.eventRoots[chain.Cursor.JournalID] = root
			}
			encoder := json.NewEncoder(root)
			for _, event := range chains[i].Events {
				if err := encoder.Encode(memberEventProjection(event)); err != nil {
					return err
				}
				v.recent[chain.Cursor.JournalID] = append(v.recent[chain.Cursor.JournalID], event.liveTransition)
				if len(v.recent[chain.Cursor.JournalID]) > 20 {
					v.recent[chain.Cursor.JournalID] = v.recent[chain.Cursor.JournalID][len(v.recent[chain.Cursor.JournalID])-20:]
				}
			}
		}
	}
	if len(usedHandoffs) != len(doc.Handoffs) {
		return errors.New("durable-transition-journal contains an unpaired member generation handoff")
	}
	for _, handoff := range doc.Handoffs {
		root := v.eventRoots[handoff.OldJournalID]
		if root == nil || hex.EncodeToString(root.Sum(nil)) != handoff.OldEventsSHA256 {
			return errors.New("durable-transition-journal handoff old events differ from retained history")
		}
	}
	if len(seen) != len(v.prior) {
		return errors.New("durable-transition-journal lost a prior chain")
	}
	v.handoffs = append(v.handoffs, doc.Handoffs[len(v.handoffs):]...)
	v.versionThreeSeen = doc.Version == 3
	v.nextGeneration++
	return nil
}

func validDurableReseedHandoff(h durableReseedHandoff, doc durableArchiveDocument) bool {
	return slices.Contains(doc.Members, h.NodeID) && slices.Contains(doc.Members, h.SourceNode) &&
		h.NodeID != h.SourceNode && h.Incarnation != "" && h.SeedTerm > 0 && validDurableJournalID(h.OldJournalID) &&
		validDurableJournalID(h.NewJournalID) && h.OldJournalID != h.NewJournalID &&
		validDurableDigest(h.OldCommittedDigest) && validDurableDigest(h.OldEventsSHA256) &&
		validDurableDigest(h.OldMemberInventorySHA256) && validDurableDigest(h.NewBaselineDigest) &&
		validDurableDigest(h.NewMemberInventorySHA256) && validDurableDigest(h.SourceInventorySHA256) &&
		validDurableDigest(h.SeedManifestSHA256) && validDurableDigest(h.FencingEvidenceSHA256) &&
		!h.RetiredFrozenAt.IsZero() && !h.SourceFrozenAt.IsZero() && !h.ReplacementFrozenAt.IsZero() &&
		!h.CommittedAt.IsZero() && !h.RetiredFrozenAt.After(h.ReplacementFrozenAt) &&
		!h.SourceFrozenAt.After(h.ReplacementFrozenAt) && !h.CommittedAt.Before(h.ReplacementFrozenAt)
}

// Replay one bounded document at a time. Recovery memory is independent of the
// number of closed event segments; only chain cursors and recent UI events stay.
func replayDurableArchive(path string, current durableArchiveDocument, environment, cluster string, members []string) (map[string][]liveTransition, uint64, error) {
	v := newDurableArchiveVerifier(environment, cluster, members)
	var closedBytes uint64
	for generation := uint64(0); generation < current.Generation; generation++ {
		segmentPath := durableArchiveSegmentPath(path, generation)
		segment, err := readDurableArchiveDocument(segmentPath)
		if err != nil {
			return nil, 0, err
		}
		if err := v.verify(segment); err != nil {
			return nil, 0, err
		}
		info, err := os.Stat(segmentPath)
		if err != nil {
			return nil, 0, err
		}
		closedBytes += uint64(info.Size())
	}
	if err := v.verify(current); err != nil {
		return nil, 0, err
	}
	return v.recent, closedBytes, nil
}

func (a *durableArchive) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lock == nil {
		return nil
	}
	err := a.lock.Close()
	a.lock = nil
	return err
}

func (a *durableArchive) cursor(node, incarnation string) *durableTransitionCursor {
	a.mu.Lock()
	defer a.mu.Unlock()
	chains := a.doc.Chains[node]
	if len(chains) == 0 || chains[len(chains)-1].Incarnation != incarnation {
		return nil
	}
	cursor := chains[len(chains)-1].Cursor
	return &cursor
}

func (a *durableArchive) capture(node string, page durableTransitionPage, cursor durableTransitionCursor, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	doc, failure := prepareDurableCapture(a.doc, node, page, cursor, now)
	if failure != "" {
		a.problems[node] = failure
		return false
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		a.lastErr = "journal_write_failed"
		return false
	}
	if len(encoded) > a.segmentTarget && len(page.Events) > 0 {
		if err := a.rotateLocked(); err != nil {
			a.lastErr = "journal_write_failed"
			log.Printf("HA durable transition journal rollover failed: %v", err)
			return false
		}
		doc, failure = prepareDurableCapture(a.doc, node, page, cursor, now)
		if failure != "" {
			a.problems[node] = failure
			return false
		}
		encoded, err = json.Marshal(doc)
	}
	if err != nil || len(encoded)+1 > durableArchiveLimit {
		a.lastErr = "journal_full"
		return false
	}
	if err := persistArchive(a.path, doc); err != nil {
		if a.lastErr == "" {
			log.Printf("HA durable transition journal persistence failed: %v", err)
		}
		a.lastErr = "journal_write_failed"
		return false
	}
	a.doc, a.lastErr, a.manifestBytes = doc, "", uint64(len(encoded)+1)
	for _, event := range page.Events {
		a.recent[cursor.JournalID] = append(a.recent[cursor.JournalID], event.liveTransition)
		if len(a.recent[cursor.JournalID]) > 20 {
			a.recent[cursor.JournalID] = a.recent[cursor.JournalID][len(a.recent[cursor.JournalID])-20:]
		}
	}
	delete(a.problems, node)
	return true
}

func prepareDurableCapture(base durableArchiveDocument, node string, page durableTransitionPage, cursor durableTransitionCursor, now time.Time) (durableArchiveDocument, string) {
	doc := base
	doc.Chains = make(map[string][]durableStoredChain, len(base.Chains))
	for key, chains := range base.Chains {
		doc.Chains[key] = append([]durableStoredChain(nil), chains...)
	}
	if !slices.Contains(doc.Members, node) || page.NodeID != node || page.ClusterID != doc.Cluster || page.Version != 1 {
		return doc, "identity_or_schema"
	}
	chains := doc.Chains[node]
	if len(chains) == 0 || chains[len(chains)-1].Incarnation != page.Incarnation {
		for _, prior := range chains {
			if prior.Incarnation == page.Incarnation {
				return doc, "incarnation_reused"
			}
		}
		for _, otherChains := range doc.Chains {
			for _, prior := range otherChains {
				if prior.Cursor.JournalID == page.NextCursor.JournalID {
					return doc, "journal_id_reused"
				}
			}
		}
		baseline := durableTransitionCursor{JournalID: page.NextCursor.JournalID, BaselineDigest: page.BaselineDigest,
			Digest: page.BaselineDigest, Role: page.BaselineRole, Term: page.BaselineTerm, PromisedTerm: page.BaselinePromised}
		chains = append(chains, durableStoredChain{NodeID: node, Incarnation: page.Incarnation, BaselineKind: page.BaselineKind,
			BaselineRole: page.BaselineRole, BaselineTerm: page.BaselineTerm, BaselinePromised: page.BaselinePromised,
			StartCursor: baseline, Cursor: baseline})
	}
	chain := &chains[len(chains)-1]
	if cursor.Sequence < chain.Cursor.Sequence || cursor.Sequence-chain.Cursor.Sequence != uint64(len(page.Events)) ||
		chain.Cursor.JournalID != cursor.JournalID || chain.Cursor.BaselineDigest != cursor.BaselineDigest ||
		chain.BaselineKind != page.BaselineKind || chain.BaselineRole != page.BaselineRole ||
		chain.BaselineTerm != page.BaselineTerm || chain.BaselinePromised != page.BaselinePromised ||
		len(page.Events) > 0 && page.Events[0].At.Before(chain.LastEventAt) {
		return doc, "chain_mismatch"
	}
	chain.Events = append(append([]durableTransitionEvent(nil), chain.Events...), page.Events...)
	chain.Cursor, chain.CommittedSequence, chain.HeadDigest, chain.ObservedAt = cursor, page.CommittedSequence, page.HeadDigest, now
	if len(page.Events) > 0 {
		chain.LastEventAt = page.Events[len(page.Events)-1].At
	}
	if !validStoredChain(*chain) {
		return doc, "chain_mismatch"
	}
	doc.Chains[node] = chains
	return doc, ""
}

func (a *durableArchive) rotateLocked() error {
	hasEvents := false
	for _, chains := range a.doc.Chains {
		for _, chain := range chains {
			hasEvents = hasEvents || len(chain.Events) > 0
		}
	}
	if !hasEvents {
		return nil
	}
	segmentPath := durableArchiveSegmentPath(a.path, a.doc.Generation)
	if _, err := os.Lstat(segmentPath); errors.Is(err, os.ErrNotExist) {
		if err := persistArchive(segmentPath, a.doc); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		segment, err := readDurableArchiveDocument(segmentPath)
		if err != nil || !reflect.DeepEqual(segment, a.doc) {
			return errors.New("durable-transition-journal rollover segment conflicts with current manifest")
		}
	}
	next := rolledDurableArchiveDocument(a.doc)
	encoded, err := json.Marshal(next)
	if err != nil || len(encoded)+1 > durableArchiveLimit {
		return errors.New("durable-transition-journal rollover manifest exceeds size bound")
	}
	if err := persistArchive(a.path, next); err != nil {
		return err
	}
	a.closedBytes += a.manifestBytes
	a.manifestBytes = uint64(len(encoded) + 1)
	a.doc = next
	return nil
}

func (a *durableArchive) fail(node, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.problems[node] = reason
}

func (a *durableArchive) view() durableArchiveView {
	a.mu.Lock()
	defer a.mu.Unlock()
	view := durableArchiveView{Status: "caught_up", ClosedSegments: a.doc.Generation,
		ManifestBytes: a.manifestBytes, StorageBytes: a.closedBytes + a.manifestBytes}
	for _, node := range a.doc.Members {
		entry := durableArchiveMemberView{NodeID: node, Status: "not_started", Failure: a.problems[node]}
		chains := a.doc.Chains[node]
		if len(chains) > 0 {
			chain := chains[len(chains)-1]
			entry.Incarnation, entry.JournalID, entry.BaselineKind = chain.Incarnation, chain.Cursor.JournalID, chain.BaselineKind
			entry.StoredSequence, entry.ObservedHead, entry.EventsRetained, entry.ObservedAt = chain.Cursor.Sequence, chain.CommittedSequence, chain.Cursor.Sequence, chain.ObservedAt
			entry.RecentEvents = append([]liveTransition(nil), a.recent[chain.Cursor.JournalID]...)
			if chain.Cursor.Sequence < chain.CommittedSequence {
				entry.Status = "catching_up"
			} else {
				entry.Status = "caught_up"
			}
			if time.Since(chain.ObservedAt) > 45*time.Second {
				entry.Status = "stale"
			}
		}
		if entry.Failure != "" {
			entry.Status = "unavailable"
		}
		if a.lastErr != "" {
			entry.Status = a.lastErr
		}
		if entry.Status != "caught_up" {
			view.Status = "partial"
		}
		view.Members = append(view.Members, entry)
	}
	return view
}

func (s *server) pollDurableArchive(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		s.pollDurableOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *server) pollDurableOnce(ctx context.Context) {
	if s.durableArchive == nil {
		return
	}
	for _, node := range s.members {
		if ctx.Err() != nil {
			return
		}
		collector := s.statusCollectors[node]
		status := collector.fetch(ctx, s.cluster)
		if status.Error != "" {
			s.durableArchive.fail(node, "member_status_"+status.Error)
			continue
		}
		cursor := s.durableArchive.cursor(node, status.Incarnation)
		for pageNumber := 0; pageNumber < 2; pageNumber++ {
			page, next, failure := collector.fetchDurablePage(ctx, s.cluster, status.Incarnation, cursor)
			if failure != "" {
				// A same-incarnation reseed creates a fresh member-local journal.
				// Inspect its authenticated baseline for diagnosis, but never
				// replace the stored cursor without an explicit evidence handoff.
				if failure == "cursor_conflict" && cursor != nil {
					fresh, _, freshFailure := collector.fetchDurablePage(ctx, s.cluster, status.Incarnation, nil)
					if freshFailure == "" && fresh.NextCursor.JournalID != cursor.JournalID {
						failure = "journal_changed_requires_reconciliation"
					}
				}
				s.durableArchive.fail(node, failure)
				break
			}
			if cursor != nil && page.NextCursor.JournalID != cursor.JournalID {
				s.durableArchive.fail(node, "journal_changed_requires_reconciliation")
				break
			}
			if !s.durableArchive.capture(node, page, next, time.Now().UTC()) || !page.HasMore {
				break
			}
			cursor = &next
		}
	}
}

func (s *server) durableArchiveResponse(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(s.durableArchiveView())
}

func (s *server) durableArchiveView() durableArchiveView {
	if s.durableArchive == nil {
		return durableArchiveView{Status: "not_configured"}
	}
	return s.durableArchive.view()
}
