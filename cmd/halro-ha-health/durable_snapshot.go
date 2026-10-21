package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/akz142857/Halro/internal/replication"
)

type durableSnapshotFile struct {
	Name   string `json:"name"`
	Bytes  uint64 `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type durableSnapshotChain struct {
	NodeID            string    `json:"node_id"`
	Incarnation       string    `json:"incarnation"`
	BaselineKind      string    `json:"baseline_kind"`
	JournalID         string    `json:"journal_id"`
	BaselineDigest    string    `json:"baseline_digest"`
	StoredSequence    uint64    `json:"stored_sequence"`
	CommittedSequence uint64    `json:"committed_sequence"`
	StoredDigest      string    `json:"stored_digest"`
	HeadDigest        string    `json:"head_digest"`
	EventsSHA256      string    `json:"events_sha256"`
	ObservedAt        time.Time `json:"observed_at"`
	Coverage          string    `json:"coverage"`
}

// This is an inventory of a frozen local snapshot, not an external archive
// receipt. Store its SHA-256 root outside the health service's failure domain.
type durableSnapshotReport struct {
	Version         int                    `json:"version"`
	Status          string                 `json:"status"`
	Environment     string                 `json:"environment"`
	Cluster         string                 `json:"cluster"`
	Members         []string               `json:"members"`
	ClosedSegments  uint64                 `json:"closed_segments"`
	StorageBytes    uint64                 `json:"storage_bytes"`
	InventorySHA256 string                 `json:"inventory_sha256"`
	Files           []durableSnapshotFile  `json:"files"`
	Chains          []durableSnapshotChain `json:"chains"`
	Handoffs        []durableReseedHandoff `json:"handoffs,omitempty"`
	NotStarted      []string               `json:"not_started_members,omitempty"`
}

// Use the member page's public schema for the cross-snapshot event hash.
func memberEventProjection(event durableTransitionEvent) replication.DurableTransitionEvidence {
	return replication.DurableTransitionEvidence{
		MemberTransitionEvent: replication.MemberTransitionEvent{
			Sequence: event.Sequence, At: event.At, Kind: event.Kind,
			FromRole: replication.Role(event.FromRole), ToRole: replication.Role(event.ToRole),
			FromTerm: event.FromTerm, ToTerm: event.ToTerm,
			FromPromisedTerm: event.FromPromisedTerm, ToPromisedTerm: event.ToPromisedTerm,
		},
		PreviousDigest: event.PreviousDigest, Digest: event.Digest,
	}
}

// verifyDurableSnapshot reads without repairing or changing the snapshot.
// Call only on a stopped-service copy or an atomic filesystem snapshot.
func verifyDurableSnapshot(path, environment, cluster string, members []string) (durableSnapshotReport, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || environment == "" || cluster == "" {
		return durableSnapshotReport{}, errors.New("snapshot verification requires a clean absolute path, environment and cluster")
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0o022 != 0 {
		return durableSnapshotReport{}, errors.New("snapshot parent must be a directory without group/world write access")
	}
	// A copied lock file is harmless, but refuse the live collector while it
	// owns the source. A lock-free directory still needs an external freeze.
	if lockInfo, err := os.Lstat(path + ".lock"); err == nil {
		if !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm() != 0o600 {
			return durableSnapshotReport{}, errors.New("snapshot lock must be a private regular file")
		}
		lock, err := os.Open(path + ".lock")
		if err != nil {
			return durableSnapshotReport{}, err
		}
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return durableSnapshotReport{}, errors.New("snapshot source is owned by a running collector")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return durableSnapshotReport{}, err
	}
	copyMembers := append([]string(nil), members...)
	slices.Sort(copyMembers)
	if len(copyMembers) < 2 || len(copyMembers) > 3 {
		return durableSnapshotReport{}, errors.New("snapshot requires two or three members")
	}
	for i, member := range copyMembers {
		if member == "" || i > 0 && member == copyMembers[i-1] {
			return durableSnapshotReport{}, errors.New("snapshot members must be unique and nonempty")
		}
	}
	current, currentData, err := readDurableArchivePayload(path)
	if err != nil {
		return durableSnapshotReport{}, err
	}
	if current.Version != 2 && current.Version != 3 {
		return durableSnapshotReport{}, errors.New("snapshot must use a supported collector format")
	}
	generations, err := durableArchiveSegments(path)
	if err != nil {
		return durableSnapshotReport{}, err
	}
	if uint64(len(generations)) != current.Generation {
		return durableSnapshotReport{}, errors.New("snapshot has a missing, extra or unfinished segment")
	}
	report := durableSnapshotReport{Version: 1, Status: "local_files_verified", Environment: environment, Cluster: cluster,
		Members: copyMembers, ClosedSegments: current.Generation}
	root := sha256.New()
	addFile := func(name string, data []byte) {
		digest := sha256.Sum256(data)
		entry := durableSnapshotFile{Name: name, Bytes: uint64(len(data)), SHA256: hex.EncodeToString(digest[:])}
		report.Files = append(report.Files, entry)
		report.StorageBytes += entry.Bytes
		_, _ = fmt.Fprintf(root, "%s\x00%d\x00%s\n", entry.Name, entry.Bytes, entry.SHA256)
	}
	verifier := newDurableArchiveVerifier(environment, cluster, copyMembers)
	eventRoots := make(map[string]hash.Hash)
	appendEvents := func(doc durableArchiveDocument) error {
		for _, chains := range doc.Chains {
			for _, chain := range chains {
				journalID := chain.Cursor.JournalID
				root := eventRoots[journalID]
				if root == nil {
					root = sha256.New()
					eventRoots[journalID] = root
				}
				encoder := json.NewEncoder(root)
				for _, event := range chain.Events {
					if err := encoder.Encode(memberEventProjection(event)); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	for _, generation := range generations {
		segmentPath := durableArchiveSegmentPath(path, generation)
		segment, data, err := readDurableArchivePayload(segmentPath)
		if err != nil {
			return durableSnapshotReport{}, err
		}
		if err := verifier.verify(segment); err != nil {
			return durableSnapshotReport{}, err
		}
		if err := appendEvents(segment); err != nil {
			return durableSnapshotReport{}, err
		}
		addFile(filepath.Base(segmentPath), data)
	}
	if err := verifier.verify(current); err != nil {
		return durableSnapshotReport{}, err
	}
	if err := appendEvents(current); err != nil {
		return durableSnapshotReport{}, err
	}
	addFile(filepath.Base(path), currentData)
	for _, node := range copyMembers {
		if len(current.Chains[node]) == 0 {
			report.NotStarted = append(report.NotStarted, node)
		}
		for _, chain := range current.Chains[node] {
			eventsRoot, ok := eventRoots[chain.Cursor.JournalID]
			if !ok {
				return durableSnapshotReport{}, errors.New("snapshot transition chain has no event root")
			}
			coverage := "caught_up_to_observed_head"
			if chain.Cursor.Sequence < chain.CommittedSequence {
				coverage = "catching_up"
			}
			report.Chains = append(report.Chains, durableSnapshotChain{NodeID: node, Incarnation: chain.Incarnation,
				BaselineKind: chain.BaselineKind, JournalID: chain.Cursor.JournalID,
				BaselineDigest: chain.Cursor.BaselineDigest, StoredSequence: chain.Cursor.Sequence,
				CommittedSequence: chain.CommittedSequence, StoredDigest: chain.Cursor.Digest,
				HeadDigest: chain.HeadDigest, EventsSHA256: hex.EncodeToString(eventsRoot.Sum(nil)),
				ObservedAt: chain.ObservedAt, Coverage: coverage})
		}
	}
	report.InventorySHA256 = hex.EncodeToString(root.Sum(nil))
	report.Handoffs = append([]durableReseedHandoff(nil), current.Handoffs...)
	for _, handoff := range report.Handoffs {
		root, ok := eventRoots[handoff.OldJournalID]
		if !ok || hex.EncodeToString(root.Sum(nil)) != handoff.OldEventsSHA256 {
			return durableSnapshotReport{}, errors.New("snapshot handoff old event sequence differs from retained history")
		}
	}
	return report, nil
}
