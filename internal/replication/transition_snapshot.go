package replication

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
)

type TransitionSnapshotFile struct {
	Name   string `json:"name"`
	Bytes  uint64 `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// TransitionSnapshotReport describes only the authenticated member-state and
// transition-journal files in one frozen copy. The inventory root needs an
// independent immutable receipt before it is durable archive evidence.
type TransitionSnapshotReport struct {
	Version         int                      `json:"version"`
	Status          string                   `json:"status"`
	ClusterID       string                   `json:"cluster_id"`
	Incarnation     string                   `json:"incarnation"`
	NodeID          string                   `json:"node_id"`
	JournalID       string                   `json:"journal_id"`
	BaselineKind    string                   `json:"baseline_kind"`
	BaselineDigest  string                   `json:"baseline_digest"`
	Committed       uint64                   `json:"committed_sequence"`
	CommittedDigest string                   `json:"committed_digest"`
	EventsSHA256    string                   `json:"events_sha256"`
	Segments        int                      `json:"segments"`
	StorageBytes    uint64                   `json:"storage_bytes"`
	InventorySHA256 string                   `json:"inventory_sha256"`
	Files           []TransitionSnapshotFile `json:"files"`
}

// VerifyTransitionSnapshot authenticates a frozen version-3 state and every
// member journal segment without opening any file for writing. A pending
// intent is rejected so the report never treats it as archived history.
func VerifyTransitionSnapshot(statePath string, key []byte, expected MemberState) (TransitionSnapshotReport, error) {
	if !filepath.IsAbs(statePath) || filepath.Clean(statePath) != statePath || expected.Version != TransitionStateVersion {
		return TransitionSnapshotReport{}, errors.New("transition snapshot requires a clean absolute version-3 state path")
	}
	parent, err := os.Lstat(filepath.Dir(statePath))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0o022 != 0 {
		return TransitionSnapshotReport{}, errors.New("transition snapshot parent must be a private directory")
	}
	info, err := os.Lstat(statePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() == 0 || info.Size() > MaxMemberStateJSON {
		return TransitionSnapshotReport{}, errors.New("transition snapshot state must be a private bounded regular file")
	}
	stateBytes, err := os.ReadFile(statePath)
	if err != nil || int64(len(stateBytes)) != info.Size() {
		return TransitionSnapshotReport{}, errors.New("transition snapshot state changed while being read")
	}
	state, err := UnmarshalState(stateBytes, key)
	if err != nil || !reflect.DeepEqual(state, expected) {
		return TransitionSnapshotReport{}, errors.New("transition snapshot state is not the authenticated expected member state")
	}
	journal, err := openTransitionJournal(TransitionJournalPath(statePath), key, state, true)
	if err != nil {
		return TransitionSnapshotReport{}, err
	}
	defer journal.Close()
	if journal.pending != nil {
		return TransitionSnapshotReport{}, errors.New("transition snapshot contains an uncommitted intent")
	}
	// Hash the public projection of every MAC-authenticated committed event.
	// Page rechecks each bounded range against the authenticated journal; the
	// digest is over JSON records with one newline after each event.
	eventsRoot := sha256.New()
	eventsEncoder := json.NewEncoder(eventsRoot)
	for sequence, digest := uint64(0), publicTransitionDigest(journal.baseline.MAC); sequence < journal.committed; {
		page, err := journal.Page(sequence, digest, 64)
		if err != nil || len(page.Events) == 0 {
			return TransitionSnapshotReport{}, errors.New("transition snapshot cannot re-read committed events")
		}
		for _, event := range page.Events {
			if err := eventsEncoder.Encode(event); err != nil {
				return TransitionSnapshotReport{}, err
			}
		}
		sequence, digest = page.NextCursor.Sequence, page.NextCursor.Digest
	}
	report := TransitionSnapshotReport{Version: 1, Status: "member_mac_verified", ClusterID: state.ClusterID,
		Incarnation: state.Incarnation, NodeID: state.NodeID, JournalID: journal.baseline.JournalID,
		BaselineKind: journal.baseline.Kind, BaselineDigest: publicTransitionDigest(journal.baseline.MAC),
		Committed: journal.committed, CommittedDigest: publicTransitionDigest(journal.committedRec.MAC),
		EventsSHA256: hex.EncodeToString(eventsRoot.Sum(nil)), Segments: len(journal.segments)}
	root := sha256.New()
	addFile := func(name string, size uint64, sum [sha256.Size]byte) {
		entry := TransitionSnapshotFile{Name: name, Bytes: size, SHA256: hex.EncodeToString(sum[:])}
		report.Files = append(report.Files, entry)
		report.StorageBytes += size
		_, _ = fmt.Fprintf(root, "%s\x00%d\x00%s\n", entry.Name, entry.Bytes, entry.SHA256)
	}
	addFile(filepath.Base(statePath), uint64(len(stateBytes)), sha256.Sum256(stateBytes))
	for _, segment := range journal.segments {
		addFile(filepath.Base(segment.path), uint64(segment.bytes), segment.sha256)
	}
	report.InventorySHA256 = hex.EncodeToString(root.Sum(nil))
	return report, nil
}

// ReadVerifiedTransitionSnapshotPage reads a bounded committed tail from a
// frozen member copy. The supplied cursor must belong to the authenticated
// journal and its digest must match the requested sequence. Rechecking the
// inventory rejects a copy that changed during this read; freezing the copy
// remains the caller's responsibility.
func ReadVerifiedTransitionSnapshotPage(statePath string, key []byte, expected MemberState, cursor DurableTransitionCursor) (TransitionSnapshotReport, DurableTransitionPage, error) {
	report, err := VerifyTransitionSnapshot(statePath, key, expected)
	if err != nil {
		return TransitionSnapshotReport{}, DurableTransitionPage{}, err
	}
	if cursor.JournalID != report.JournalID || cursor.Sequence > report.Committed {
		return TransitionSnapshotReport{}, DurableTransitionPage{}, ErrInvalidTransitionCursor
	}
	journal, err := openTransitionJournal(TransitionJournalPath(statePath), key, expected, true)
	if err != nil {
		return TransitionSnapshotReport{}, DurableTransitionPage{}, err
	}
	page, pageErr := journal.Page(cursor.Sequence, cursor.Digest, 64)
	closeErr := journal.Close()
	if pageErr != nil {
		return TransitionSnapshotReport{}, DurableTransitionPage{}, pageErr
	}
	if closeErr != nil {
		return TransitionSnapshotReport{}, DurableTransitionPage{}, closeErr
	}
	again, err := VerifyTransitionSnapshot(statePath, key, expected)
	if err != nil {
		return TransitionSnapshotReport{}, DurableTransitionPage{}, err
	}
	if !reflect.DeepEqual(again, report) || page.CommittedSequence != report.Committed || page.HeadDigest != report.CommittedDigest {
		return TransitionSnapshotReport{}, DurableTransitionPage{}, errors.New("transition snapshot changed while reading committed tail")
	}
	return report, page, nil
}

// VerifyTransitionSnapshotSeedOrigin binds a replacement journal's authenticated
// baseline to the exact version-2 Replica state produced by seed-install.
// The current member may have advanced since migration; the frozen snapshot
// and its complete event inventory must remain stable during verification.
func VerifyTransitionSnapshotSeedOrigin(statePath string, key []byte, current, seeded MemberState) (TransitionSnapshotReport, error) {
	if seeded.Version != StateVersion || seeded.Role != RoleReplica || seeded.Validate() != nil ||
		current.Version != TransitionStateVersion || current.ClusterID != seeded.ClusterID ||
		current.Incarnation != seeded.Incarnation || current.NodeID != seeded.NodeID {
		return TransitionSnapshotReport{}, errors.New("seed origin requires matching authenticated current and version-2 Replica identities")
	}
	report, err := VerifyTransitionSnapshot(statePath, key, current)
	if err != nil {
		return TransitionSnapshotReport{}, err
	}
	if report.BaselineKind != "legacy_baseline" {
		return TransitionSnapshotReport{}, errors.New("replacement journal is not a migrated version-2 baseline")
	}
	journal, err := openTransitionJournal(TransitionJournalPath(statePath), key, current, true)
	if err != nil {
		return TransitionSnapshotReport{}, err
	}
	seedMAC, err := memberStateMAC(seeded, key)
	if err != nil {
		_ = journal.Close()
		return TransitionSnapshotReport{}, err
	}
	matched := hmac.Equal([]byte(journal.baseline.BaselineStateMAC), []byte(digestText(seedMAC)))
	if err := journal.Close(); err != nil {
		return TransitionSnapshotReport{}, err
	}
	if !matched {
		return TransitionSnapshotReport{}, errors.New("replacement journal baseline is not the approved seeded Replica state")
	}
	again, err := VerifyTransitionSnapshot(statePath, key, current)
	if err != nil || !reflect.DeepEqual(report, again) {
		return TransitionSnapshotReport{}, errors.New("replacement member snapshot changed during seed-origin verification")
	}
	return report, nil
}
