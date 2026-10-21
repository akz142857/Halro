package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/akz142857/Halro/internal/app"
	halroconfig "github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
)

type directMemberSnapshot struct {
	NodeID      string `json:"node_id"`
	Incarnation string `json:"incarnation,omitempty"`
	JournalID   string `json:"journal_id,omitempty"`
	Config      string `json:"config"`
	SnapshotDir string `json:"snapshot_dir"`
}

const maxDirectSnapshotManifest = 1 << 20

// verifyMemberSnapshotHeads independently re-authenticates frozen member
// copies with their Master Keys. Manifest v1 covers current chains; v2 covers
// distinct incarnations; v3 identifies every retained member generation by
// (node, incarnation, journal ID).
func verifyMemberSnapshotHeads(ctx context.Context, collector durableSnapshotReport, manifestPath string) (snapshotHeadComparison, error) {
	if collector.Version != 1 || collector.Status != "local_files_verified" ||
		len(collector.Members) < 2 || len(collector.Members) > 3 {
		return snapshotHeadComparison{}, errors.New("collector snapshot must first pass local file verification")
	}
	data, err := readPrivateSnapshotInput(manifestPath, maxDirectSnapshotManifest)
	if err != nil {
		return snapshotHeadComparison{}, err
	}
	var manifest struct {
		Version int                    `json:"version"`
		Members []directMemberSnapshot `json:"members"`
	}
	if err := decodeExactJSON(data, &manifest); err != nil || (manifest.Version != 1 && manifest.Version != 2 && manifest.Version != 3) ||
		manifest.Version == 1 && len(manifest.Members) != len(collector.Members) ||
		manifest.Version >= 2 && len(manifest.Members) != len(collector.Chains) ||
		manifest.Version == 2 && len(collector.Handoffs) != 0 {
		return snapshotHeadComparison{}, errors.New("direct member snapshot manifest must cover the exact current or all-chain inventory")
	}
	seen := make(map[snapshotChainKey]bool, len(manifest.Members))
	paths := make(map[string]bool, len(manifest.Members))
	var evidence []memberSnapshotEvidence
	for _, entry := range manifest.Members {
		key := snapshotChainKey{NodeID: entry.NodeID, Incarnation: entry.Incarnation}
		if manifest.Version == 1 {
			key.Incarnation = ""
		} else if manifest.Version == 3 {
			key.JournalID = entry.JournalID
		}
		if !slices.Contains(collector.Members, entry.NodeID) || seen[key] || paths[entry.SnapshotDir] ||
			manifest.Version == 1 && (entry.Incarnation != "" || entry.JournalID != "") ||
			manifest.Version == 2 && (entry.Incarnation == "" || entry.JournalID != "") ||
			manifest.Version == 3 && (entry.Incarnation == "" || !validDurableJournalID(entry.JournalID)) ||
			!filepath.IsAbs(entry.Config) || filepath.Clean(entry.Config) != entry.Config ||
			!filepath.IsAbs(entry.SnapshotDir) || filepath.Clean(entry.SnapshotDir) != entry.SnapshotDir {
			return snapshotHeadComparison{}, errors.New("direct member snapshot manifest has duplicate, unknown or non-absolute entries")
		}
		seen[key], paths[entry.SnapshotDir] = true, true
		cfg, err := halroconfig.Load(entry.Config, halroconfig.LoadOptions{SkipListenerValidation: true})
		if err != nil {
			return snapshotHeadComparison{}, fmt.Errorf("load archived member configuration for %s: %w", entry.NodeID, err)
		}
		if cfg.Replication == nil || cfg.Replication.NodeID != entry.NodeID || cfg.Replication.ClusterID != collector.Cluster {
			return snapshotHeadComparison{}, fmt.Errorf("archived member configuration identity mismatch for %s", entry.NodeID)
		}
		report, err := app.VerifyMemberTransitionSnapshot(ctx, cfg, entry.SnapshotDir)
		if err != nil {
			return snapshotHeadComparison{}, fmt.Errorf("verify archived member %s: %w", entry.NodeID, err)
		}
		if manifest.Version >= 2 && report.Incarnation != entry.Incarnation ||
			manifest.Version == 3 && report.JournalID != entry.JournalID {
			return snapshotHeadComparison{}, fmt.Errorf("archived member incarnation mismatch for %s/%s", entry.NodeID, entry.Incarnation)
		}
		evidence = append(evidence, memberSnapshotEvidence{Report: report})
	}
	var comparison snapshotHeadComparison
	if manifest.Version == 1 {
		comparison, err = compareSnapshotHeads(collector, evidence, "member_authenticated_current_chain_match")
	} else {
		comparison, err = compareAllSnapshotChains(collector, evidence)
	}
	if err != nil {
		return snapshotHeadComparison{}, err
	}
	for _, item := range evidence {
		comparison.VerifiedMemberReports = append(comparison.VerifiedMemberReports, item.Report)
	}
	slices.SortFunc(comparison.VerifiedMemberReports, func(a, b replication.TransitionSnapshotReport) int {
		if a.NodeID != b.NodeID {
			return strings.Compare(a.NodeID, b.NodeID)
		}
		if a.Incarnation != b.Incarnation {
			return strings.Compare(a.Incarnation, b.Incarnation)
		}
		return strings.Compare(a.JournalID, b.JournalID)
	})
	return comparison, nil
}
