package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/akz142857/Halro/internal/app"
	halroconfig "github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
)

type reseedHandoffCommitReport struct {
	Status                   string `json:"status"`
	NodeID                   string `json:"node_id"`
	Incarnation              string `json:"incarnation"`
	OldJournalID             string `json:"old_journal_id"`
	NewJournalID             string `json:"new_journal_id"`
	CollectorInventorySHA256 string `json:"collector_inventory_sha256"`
}

// commitReseedHandoff runs only with the collector stopped. The version-3
// manifest persists the old chain, its authenticated handoff record, and the
// new baseline in one atomic replacement. The new journal's events are then
// collected normally from sequence zero; they are never spliced to the old
// chain's digest.
func commitReseedHandoff(ctx context.Context, inputPath, archivePath string) (reseedHandoffCommitReport, error) {
	proof, err := preflightReseedHandoff(ctx, inputPath)
	if err != nil {
		return reseedHandoffCommitReport{}, err
	}
	data, err := readPrivateSnapshotInput(inputPath, maxDirectSnapshotManifest)
	if err != nil {
		return reseedHandoffCommitReport{}, err
	}
	var input reseedHandoffPreflightInput
	if err := decodeExactJSON(data, &input); err != nil {
		return reseedHandoffCommitReport{}, err
	}
	members := append([]string(nil), input.Members...)
	slices.Sort(members)
	archive, err := openDurableArchive(archivePath, input.Environment, input.Cluster, members)
	if err != nil {
		return reseedHandoffCommitReport{}, err
	}
	defer archive.Close()
	frozen, err := verifyDurableSnapshot(input.Collector, input.Environment, input.Cluster, members)
	if err != nil || frozen.InventorySHA256 != proof.CollectorInventorySHA256 {
		return reseedHandoffCommitReport{}, errors.New("frozen collector changed after handoff preflight")
	}
	if err := matchLiveArchiveFiles(archivePath, frozen.Files); err != nil {
		return reseedHandoffCommitReport{}, fmt.Errorf("stopped collector differs from frozen preflight copy: %w", err)
	}
	chains := archive.doc.Chains[proof.NodeID]
	if len(chains) == 0 || chains[len(chains)-1].Incarnation != proof.Incarnation ||
		chains[len(chains)-1].Cursor.JournalID != proof.OldJournalID ||
		chains[len(chains)-1].Cursor.Sequence != proof.OldCommittedSequence ||
		chains[len(chains)-1].Cursor.Digest != proof.OldCommittedDigest {
		return reseedHandoffCommitReport{}, errors.New("stopped collector old head differs from handoff preflight")
	}
	cfg, err := halroconfig.Load(input.Replacement.Config, halroconfig.LoadOptions{SkipListenerValidation: true})
	if err != nil {
		return reseedHandoffCommitReport{}, err
	}
	member, page, err := app.ReadMemberTransitionSnapshotPage(ctx, cfg, input.Replacement.SnapshotDir,
		replication.DurableTransitionCursor{JournalID: proof.NewJournalID, Sequence: 0, Digest: proof.NewBaselineDigest})
	if err != nil {
		return reseedHandoffCommitReport{}, fmt.Errorf("recheck replacement baseline: %w", err)
	}
	encodedPage, err := json.Marshal(page)
	if err != nil {
		return reseedHandoffCommitReport{}, err
	}
	var baselinePage durableTransitionPage
	if err := decodeExactJSON(encodedPage, &baselinePage); err != nil {
		return reseedHandoffCommitReport{}, err
	}
	if member.InventorySHA256 != proof.NewMemberInventorySHA256 || member.JournalID != proof.NewJournalID ||
		member.BaselineDigest != proof.NewBaselineDigest || baselinePage.NodeID != proof.NodeID ||
		baselinePage.Incarnation != proof.Incarnation || baselinePage.BaselineKind != "legacy_baseline" ||
		baselinePage.BaselineRole != "replica" || baselinePage.BaselineTerm != proof.SeedTerm ||
		baselinePage.BaselinePromised != proof.SeedTerm || baselinePage.NextCursor.JournalID != proof.NewJournalID ||
		baselinePage.CommittedSequence != member.Committed || baselinePage.HeadDigest != member.CommittedDigest {
		return reseedHandoffCommitReport{}, errors.New("replacement baseline differs from authenticated handoff proof")
	}
	now := time.Now().UTC()
	handoff := durableReseedHandoff{
		NodeID: proof.NodeID, Incarnation: proof.Incarnation,
		OldJournalID: proof.OldJournalID, OldCommittedSequence: proof.OldCommittedSequence,
		OldCommittedDigest: proof.OldCommittedDigest, OldEventsSHA256: proof.OldEventsSHA256,
		OldMemberInventorySHA256: proof.OldMemberInventorySHA256,
		NewJournalID:             proof.NewJournalID, NewBaselineDigest: proof.NewBaselineDigest,
		NewMemberInventorySHA256: proof.NewMemberInventorySHA256,
		SourceNode:               proof.SourceNode, SourceInventorySHA256: proof.SourceMemberInventorySHA256,
		SeedManifestSHA256: proof.SeedManifestSHA256, SeedTerm: proof.SeedTerm, SeedIndex: proof.SeedIndex,
		FencingEvidenceSHA256: proof.FencingEvidenceSHA256,
		RetiredFrozenAt:       proof.RetiredFrozenAt, SourceFrozenAt: proof.SourceFrozenAt,
		ReplacementFrozenAt: proof.ReplacementFrozenAt, CommittedAt: now,
	}
	base := archive.doc
	next := base
	next.Version = 3
	next.Handoffs = append(append([]durableReseedHandoff(nil), base.Handoffs...), handoff)
	next.Chains = make(map[string][]durableStoredChain, len(base.Chains))
	for node, existing := range base.Chains {
		next.Chains[node] = append([]durableStoredChain(nil), existing...)
	}
	baseline := durableTransitionCursor{JournalID: proof.NewJournalID, BaselineDigest: proof.NewBaselineDigest,
		Digest: proof.NewBaselineDigest, Role: baselinePage.BaselineRole,
		Term: baselinePage.BaselineTerm, PromisedTerm: baselinePage.BaselinePromised}
	next.Chains[proof.NodeID] = append(next.Chains[proof.NodeID], durableStoredChain{
		NodeID: proof.NodeID, Incarnation: proof.Incarnation, BaselineKind: baselinePage.BaselineKind,
		BaselineRole: baselinePage.BaselineRole, BaselineTerm: baselinePage.BaselineTerm,
		BaselinePromised: baselinePage.BaselinePromised, StartCursor: baseline, Cursor: baseline,
		CommittedSequence: member.Committed, HeadDigest: member.CommittedDigest, ObservedAt: now,
	})
	encoded, err := json.Marshal(next)
	if err != nil || len(encoded)+1 > durableArchiveLimit {
		return reseedHandoffCommitReport{}, errors.New("handoff manifest exceeds collector size limit")
	}
	if _, _, err := replayDurableArchive(archivePath, next, input.Environment, input.Cluster, members); err != nil {
		return reseedHandoffCommitReport{}, fmt.Errorf("handoff fails complete collector replay: %w", err)
	}
	if err := persistArchive(archivePath, next); err != nil {
		return reseedHandoffCommitReport{}, err
	}
	return reseedHandoffCommitReport{Status: "member_generation_handoff_committed_local",
		NodeID: proof.NodeID, Incarnation: proof.Incarnation, OldJournalID: proof.OldJournalID,
		NewJournalID: proof.NewJournalID, CollectorInventorySHA256: proof.CollectorInventorySHA256}, nil
}

func matchLiveArchiveFiles(path string, frozen []durableSnapshotFile) error {
	actual, err := durableArchiveSegments(path)
	if err != nil || len(actual)+1 != len(frozen) {
		return errors.New("collector segment inventory differs")
	}
	for _, file := range frozen {
		name := filepath.Base(path)
		if file.Name != name {
			matched := false
			for _, generation := range actual {
				if filepath.Base(durableArchiveSegmentPath(path, generation)) == file.Name {
					matched = true
					break
				}
			}
			if !matched {
				return errors.New("collector contains an unexpected file")
			}
		}
		filePath := filepath.Join(filepath.Dir(path), file.Name)
		info, err := os.Lstat(filePath)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || uint64(info.Size()) != file.Bytes {
			return errors.New("collector file changed or lost private permissions")
		}
		data, err := os.ReadFile(filePath)
		if err != nil || uint64(len(data)) != file.Bytes {
			return errors.New("collector file changed while reading")
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != file.SHA256 {
			return errors.New("collector file differs from frozen preflight copy")
		}
	}
	return nil
}
