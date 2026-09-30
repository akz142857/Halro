package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/akz142857/Halro/internal/app"
	halroconfig "github.com/akz142857/Halro/internal/config"
)

type frozenHandoffMember struct {
	Config      string    `json:"config"`
	SnapshotDir string    `json:"snapshot_dir"`
	FrozenAt    time.Time `json:"frozen_at"`
}

type reseedHandoffPreflightInput struct {
	Version               int                 `json:"version"`
	Environment           string              `json:"environment"`
	Cluster               string              `json:"cluster"`
	Members               []string            `json:"members"`
	Collector             string              `json:"collector"`
	Retired               frozenHandoffMember `json:"retired"`
	Source                frozenHandoffMember `json:"source"`
	Replacement           frozenHandoffMember `json:"replacement"`
	ApprovedFilesDir      string              `json:"approved_files_dir"`
	SeedManifest          string              `json:"seed_manifest"`
	FencingEvidenceSHA256 string              `json:"fencing_evidence_sha256"`
}

// This report proves local authenticated evidence inputs agree. The operator
// supplied freeze times and fencing hash are provenance references, not a
// cryptographic verdict about fencing or immutable archival retention.
type reseedHandoffPreflightReport struct {
	Version                     int       `json:"version"`
	Status                      string    `json:"status"`
	Environment                 string    `json:"environment"`
	Cluster                     string    `json:"cluster"`
	NodeID                      string    `json:"node_id"`
	Incarnation                 string    `json:"incarnation"`
	OldJournalID                string    `json:"old_journal_id"`
	OldCommittedSequence        uint64    `json:"old_committed_sequence"`
	OldCommittedDigest          string    `json:"old_committed_digest"`
	OldEventsSHA256             string    `json:"old_events_sha256"`
	OldMemberInventorySHA256    string    `json:"old_member_inventory_sha256"`
	NewJournalID                string    `json:"new_journal_id"`
	NewBaselineDigest           string    `json:"new_baseline_digest"`
	NewMemberInventorySHA256    string    `json:"new_member_inventory_sha256"`
	SourceNode                  string    `json:"source_node"`
	SourceMemberInventorySHA256 string    `json:"source_member_inventory_sha256"`
	SeedManifestSHA256          string    `json:"seed_manifest_sha256"`
	SeedTerm                    uint64    `json:"seed_term"`
	SeedIndex                   uint64    `json:"seed_index"`
	CollectorInventorySHA256    string    `json:"collector_inventory_sha256"`
	FencingEvidenceSHA256       string    `json:"fencing_evidence_sha256"`
	RetiredFrozenAt             time.Time `json:"retired_frozen_at"`
	SourceFrozenAt              time.Time `json:"source_frozen_at"`
	ReplacementFrozenAt         time.Time `json:"replacement_frozen_at"`
	VerifiedAt                  time.Time `json:"verified_at"`
}

func preflightReseedHandoff(ctx context.Context, manifestPath string) (reseedHandoffPreflightReport, error) {
	data, err := readPrivateSnapshotInput(manifestPath, maxDirectSnapshotManifest)
	if err != nil {
		return reseedHandoffPreflightReport{}, err
	}
	var input reseedHandoffPreflightInput
	if err := decodeExactJSON(data, &input); err != nil || input.Version != 1 ||
		input.Environment == "" || input.Cluster == "" || len(input.Members) < 2 || len(input.Members) > 3 ||
		input.Retired.FrozenAt.IsZero() || input.Source.FrozenAt.IsZero() || input.Replacement.FrozenAt.IsZero() ||
		!validDurableDigest(input.FencingEvidenceSHA256) ||
		input.Retired.SnapshotDir == input.Replacement.SnapshotDir || input.Source.SnapshotDir == input.Replacement.SnapshotDir {
		return reseedHandoffPreflightReport{}, errors.New("reseed handoff preflight manifest is incomplete or invalid")
	}
	members := append([]string(nil), input.Members...)
	slices.Sort(members)
	for i, member := range members {
		if member == "" || i > 0 && member == members[i-1] {
			return reseedHandoffPreflightReport{}, errors.New("reseed handoff expected members must be unique")
		}
	}
	collector, err := verifyDurableSnapshot(input.Collector, input.Environment, input.Cluster, members)
	if err != nil {
		return reseedHandoffPreflightReport{}, fmt.Errorf("verify frozen collector: %w", err)
	}
	load := func(path string) (halroconfig.Config, error) {
		return halroconfig.Load(path, halroconfig.LoadOptions{SkipListenerValidation: true})
	}
	retiredCfg, err := load(input.Retired.Config)
	if err != nil {
		return reseedHandoffPreflightReport{}, err
	}
	sourceCfg, err := load(input.Source.Config)
	if err != nil {
		return reseedHandoffPreflightReport{}, err
	}
	replacementCfg, err := load(input.Replacement.Config)
	if err != nil {
		return reseedHandoffPreflightReport{}, err
	}
	retired, err := app.VerifyMemberTransitionSnapshot(ctx, retiredCfg, input.Retired.SnapshotDir)
	if err != nil {
		return reseedHandoffPreflightReport{}, fmt.Errorf("verify retired member: %w", err)
	}
	var oldChain *durableSnapshotChain
	for i := range collector.Chains {
		chain := &collector.Chains[i]
		if chain.NodeID == retired.NodeID && chain.Incarnation == retired.Incarnation && chain.JournalID == retired.JournalID {
			if oldChain != nil {
				return reseedHandoffPreflightReport{}, errors.New("collector repeats retired member chain")
			}
			oldChain = chain
		}
	}
	if oldChain == nil || !validSnapshotMember(collector, retired) {
		return reseedHandoffPreflightReport{}, errors.New("collector lacks the authenticated retired member identity")
	}
	if _, err := compareOneSnapshotChain(*oldChain, memberSnapshotEvidence{Report: retired}); err != nil {
		return reseedHandoffPreflightReport{}, fmt.Errorf("retired chain is not complete in collector: %w", err)
	}
	stored, err := readDurableArchiveDocument(input.Collector)
	if err != nil || len(stored.Chains[retired.NodeID]) == 0 ||
		stored.Chains[retired.NodeID][len(stored.Chains[retired.NodeID])-1].Cursor.JournalID != retired.JournalID {
		return reseedHandoffPreflightReport{}, errors.New("retired journal is not the collector's current member generation")
	}
	source, err := app.VerifySourceSeedOrigin(ctx, sourceCfg, input.Source.SnapshotDir,
		replacementCfg, input.ApprovedFilesDir, input.SeedManifest)
	if err != nil {
		return reseedHandoffPreflightReport{}, fmt.Errorf("verify stopped source Primary: %w", err)
	}
	replacement, err := app.VerifyMemberSeedOrigin(ctx, replacementCfg, input.Replacement.SnapshotDir,
		input.ApprovedFilesDir, input.SeedManifest)
	if err != nil {
		return reseedHandoffPreflightReport{}, fmt.Errorf("verify replacement seed origin: %w", err)
	}
	if !reflect.DeepEqual(source.Approval, replacement.Approval) ||
		retired.ClusterID != input.Cluster || retired.NodeID != replacement.Member.NodeID ||
		retired.Incarnation != replacement.Member.Incarnation || retired.JournalID == replacement.Member.JournalID ||
		source.SourceMember.NodeID != source.Approval.SourceNode || source.Approval.TargetNode != retired.NodeID ||
		!slices.Contains(members, retired.NodeID) || !slices.Contains(members, source.SourceMember.NodeID) ||
		input.Retired.FrozenAt.After(input.Replacement.FrozenAt) || input.Source.FrozenAt.After(input.Replacement.FrozenAt) ||
		source.Approval.ApprovedAt.After(input.Replacement.FrozenAt) ||
		stored.Chains[retired.NodeID][len(stored.Chains[retired.NodeID])-1].Cursor.PromisedTerm > source.Approval.Term {
		return reseedHandoffPreflightReport{}, errors.New("retired, source and replacement evidence do not form one ordered handoff")
	}
	return reseedHandoffPreflightReport{Version: 1, Status: "authenticated_inputs_match_handoff_uncommitted",
		Environment: input.Environment, Cluster: input.Cluster, NodeID: retired.NodeID, Incarnation: retired.Incarnation,
		OldJournalID: retired.JournalID, OldCommittedSequence: retired.Committed,
		OldCommittedDigest: retired.CommittedDigest, OldEventsSHA256: retired.EventsSHA256,
		OldMemberInventorySHA256: retired.InventorySHA256,
		NewJournalID:             replacement.Member.JournalID, NewBaselineDigest: replacement.Member.BaselineDigest,
		NewMemberInventorySHA256: replacement.Member.InventorySHA256,
		SourceNode:               source.SourceMember.NodeID, SourceMemberInventorySHA256: source.SourceMember.InventorySHA256,
		SeedManifestSHA256: source.Approval.ManifestSHA256, SeedTerm: source.Approval.Term, SeedIndex: source.Approval.Index,
		CollectorInventorySHA256: collector.InventorySHA256, FencingEvidenceSHA256: input.FencingEvidenceSHA256,
		RetiredFrozenAt: input.Retired.FrozenAt, SourceFrozenAt: input.Source.FrozenAt,
		ReplacementFrozenAt: input.Replacement.FrozenAt, VerifiedAt: time.Now().UTC()}, nil
}
