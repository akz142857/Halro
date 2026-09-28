package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/replication"
)

const (
	maxSnapshotComparisonManifest = 64 << 10
	maxMemberSnapshotReport       = 16 << 20
)

type snapshotHeadMatch struct {
	NodeID                string    `json:"node_id"`
	Incarnation           string    `json:"incarnation"`
	JournalID             string    `json:"journal_id"`
	CommittedSequence     uint64    `json:"committed_sequence"`
	CommittedDigest       string    `json:"committed_digest"`
	EventsSHA256          string    `json:"events_sha256"`
	CollectorObservedAt   time.Time `json:"collector_observed_at"`
	MemberInventorySHA256 string    `json:"member_inventory_sha256"`
	MemberReportSHA256    string    `json:"member_report_sha256,omitempty"`
}

// A snapshotHeadComparison records current-head consistency between a verified
// collector copy and member evidence. Only the direct verification mode
// authenticates the frozen member files; neither mode is an archive receipt.
type snapshotHeadComparison struct {
	Version                  int                                    `json:"version"`
	Status                   string                                 `json:"status"`
	Scope                    string                                 `json:"scope,omitempty"`
	Environment              string                                 `json:"environment"`
	Cluster                  string                                 `json:"cluster"`
	CollectorInventorySHA256 string                                 `json:"collector_inventory_sha256"`
	Members                  []snapshotHeadMatch                    `json:"members"`
	VerifiedMemberReports    []replication.TransitionSnapshotReport `json:"verified_member_reports,omitempty"`
}

type memberSnapshotEvidence struct {
	Report     replication.TransitionSnapshotReport
	ReportHash string
}

type snapshotChainKey struct {
	NodeID      string
	Incarnation string
	JournalID   string
}

func readPrivateSnapshotInput(path string, limit int64) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("snapshot report input must have a clean absolute path")
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("snapshot report input parent must be a private directory")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() == 0 || info.Size() > limit {
		return nil, errors.New("snapshot report input must be a private bounded regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil || int64(len(data)) != info.Size() {
		return nil, errors.New("snapshot report input changed while being read")
	}
	return data, nil
}

func decodeExactJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("snapshot report input has trailing data")
	}
	return nil
}

func validMemberSnapshotInventory(report replication.TransitionSnapshotReport) bool {
	if report.Segments < 1 || len(report.Files) != report.Segments+1 {
		return false
	}
	root := sha256.New()
	var total uint64
	seenNames := make(map[string]bool, len(report.Files))
	for index, file := range report.Files {
		if file.Bytes == 0 || !validDurableDigest(file.SHA256) || total > ^uint64(0)-file.Bytes || seenNames[file.Name] {
			return false
		}
		seenNames[file.Name] = true
		if index == 0 && file.Name != "state.json" || index == 1 && file.Name != "transitions.journal" ||
			index > 1 && (!strings.HasPrefix(file.Name, "transitions.journal.segment.") || strings.ContainsAny(file.Name, `/\\`)) {
			return false
		}
		total += file.Bytes
		_, _ = fmt.Fprintf(root, "%s\x00%d\x00%s\n", file.Name, file.Bytes, file.SHA256)
	}
	return total == report.StorageBytes && hex.EncodeToString(root.Sum(nil)) == report.InventorySHA256
}

func compareMemberSnapshotReports(collector durableSnapshotReport, manifestPath string) (snapshotHeadComparison, error) {
	if collector.Version != 1 || collector.Status != "local_files_verified" || !validDurableDigest(collector.InventorySHA256) ||
		len(collector.Members) < 2 || len(collector.Members) > 3 {
		return snapshotHeadComparison{}, errors.New("collector snapshot must first pass local file verification")
	}
	data, err := readPrivateSnapshotInput(manifestPath, maxSnapshotComparisonManifest)
	if err != nil {
		return snapshotHeadComparison{}, err
	}
	var manifest struct {
		Version int      `json:"version"`
		Reports []string `json:"reports"`
	}
	if err := decodeExactJSON(data, &manifest); err != nil || manifest.Version != 1 || len(manifest.Reports) != len(collector.Members) {
		return snapshotHeadComparison{}, errors.New("member snapshot comparison manifest must be version 1 with the exact member count")
	}
	var reports []memberSnapshotEvidence
	paths := make(map[string]bool, len(collector.Members))
	for _, reportPath := range manifest.Reports {
		if paths[reportPath] {
			return snapshotHeadComparison{}, errors.New("member snapshot comparison repeats a report path")
		}
		paths[reportPath] = true
		reportBytes, err := readPrivateSnapshotInput(reportPath, maxMemberSnapshotReport)
		if err != nil {
			return snapshotHeadComparison{}, err
		}
		var member replication.TransitionSnapshotReport
		if err := decodeExactJSON(reportBytes, &member); err != nil {
			return snapshotHeadComparison{}, fmt.Errorf("decode member snapshot report: %w", err)
		}
		reportHash := sha256.Sum256(reportBytes)
		reports = append(reports, memberSnapshotEvidence{Report: member, ReportHash: hex.EncodeToString(reportHash[:])})
	}
	return compareSnapshotHeads(collector, reports, "snapshot_heads_match")
}

func compareSnapshotHeads(collector durableSnapshotReport, reports []memberSnapshotEvidence, status string) (snapshotHeadComparison, error) {
	if collector.Version != 1 || collector.Status != "local_files_verified" || !validDurableDigest(collector.InventorySHA256) ||
		len(collector.Members) < 2 || len(collector.Members) > 3 || len(reports) != len(collector.Members) {
		return snapshotHeadComparison{}, errors.New("collector and member snapshots require verified complete inventories")
	}
	latest := make(map[string]durableSnapshotChain, len(collector.Members))
	for _, chain := range collector.Chains {
		latest[chain.NodeID] = chain
	}
	result := snapshotHeadComparison{Version: 1, Status: status, Environment: collector.Environment,
		Cluster: collector.Cluster, CollectorInventorySHA256: collector.InventorySHA256}
	seen := make(map[string]bool, len(collector.Members))
	for _, evidence := range reports {
		member := evidence.Report
		if !validSnapshotMember(collector, member) || seen[member.NodeID] {
			return snapshotHeadComparison{}, errors.New("member snapshot report has invalid or duplicate identity")
		}
		seen[member.NodeID] = true
		chain, ok := latest[member.NodeID]
		if !ok {
			return snapshotHeadComparison{}, fmt.Errorf("collector has no matching current transition chain for %s", member.NodeID)
		}
		match, err := compareOneSnapshotChain(chain, evidence)
		if err != nil {
			return snapshotHeadComparison{}, err
		}
		result.Members = append(result.Members, match)
	}
	if len(seen) != len(collector.Members) {
		return snapshotHeadComparison{}, errors.New("member snapshot reports do not cover the complete expected inventory")
	}
	slices.SortFunc(result.Members, func(a, b snapshotHeadMatch) int {
		if a.NodeID < b.NodeID {
			return -1
		}
		if a.NodeID > b.NodeID {
			return 1
		}
		return 0
	})
	return result, nil
}

func validSnapshotMember(collector durableSnapshotReport, member replication.TransitionSnapshotReport) bool {
	return member.Version == 1 && member.Status == "member_mac_verified" && member.ClusterID == collector.Cluster &&
		slices.Contains(collector.Members, member.NodeID) && member.Incarnation != "" &&
		validDurableDigest(member.InventorySHA256) && validDurableDigest(member.CommittedDigest) &&
		validDurableDigest(member.BaselineDigest) && validDurableDigest(member.EventsSHA256) &&
		validMemberSnapshotInventory(member)
}

func compareOneSnapshotChain(chain durableSnapshotChain, evidence memberSnapshotEvidence) (snapshotHeadMatch, error) {
	member := evidence.Report
	if chain.NodeID != member.NodeID || chain.Incarnation != member.Incarnation || chain.JournalID != member.JournalID ||
		chain.BaselineKind != member.BaselineKind || chain.BaselineDigest != member.BaselineDigest {
		return snapshotHeadMatch{}, fmt.Errorf("collector has no matching transition chain for %s/%s", member.NodeID, member.Incarnation)
	}
	if !validDurableDigest(chain.EventsSHA256) || chain.EventsSHA256 != member.EventsSHA256 ||
		chain.CommittedSequence != member.Committed || chain.HeadDigest != member.CommittedDigest ||
		chain.StoredSequence != member.Committed || chain.StoredDigest != member.CommittedDigest ||
		chain.Coverage != "caught_up_to_observed_head" || chain.ObservedAt.IsZero() {
		return snapshotHeadMatch{}, fmt.Errorf("collector transition head or events do not match frozen member %s/%s", member.NodeID, member.Incarnation)
	}
	return snapshotHeadMatch{NodeID: member.NodeID, Incarnation: member.Incarnation,
		JournalID: member.JournalID, CommittedSequence: member.Committed, CommittedDigest: member.CommittedDigest,
		EventsSHA256: member.EventsSHA256, CollectorObservedAt: chain.ObservedAt,
		MemberInventorySHA256: member.InventorySHA256, MemberReportSHA256: evidence.ReportHash}, nil
}

// compareAllSnapshotChains requires an authenticated frozen member copy for
// every incarnation retained by the collector, including the current one.
func compareAllSnapshotChains(collector durableSnapshotReport, reports []memberSnapshotEvidence) (snapshotHeadComparison, error) {
	if collector.Version != 1 || collector.Status != "local_files_verified" || !validDurableDigest(collector.InventorySHA256) ||
		len(collector.Members) < 2 || len(collector.Members) > 3 || len(collector.NotStarted) != 0 ||
		len(reports) != len(collector.Chains) {
		return snapshotHeadComparison{}, errors.New("all-chain comparison requires a verified and complete collector inventory")
	}
	chains := make(map[snapshotChainKey]durableSnapshotChain, len(collector.Chains))
	perMember := make(map[string]int, len(collector.Members))
	for _, chain := range collector.Chains {
		key := snapshotChainKey{NodeID: chain.NodeID, Incarnation: chain.Incarnation, JournalID: chain.JournalID}
		if !slices.Contains(collector.Members, chain.NodeID) || chain.Incarnation == "" || chains[key].NodeID != "" {
			return snapshotHeadComparison{}, errors.New("collector has duplicate or invalid transition chain identity")
		}
		chains[key] = chain
		perMember[chain.NodeID]++
	}
	for _, node := range collector.Members {
		if perMember[node] == 0 {
			return snapshotHeadComparison{}, errors.New("collector is missing a required member chain")
		}
	}
	result := snapshotHeadComparison{Version: 1, Status: "member_authenticated_all_collected_chains_match",
		Scope: "all_collected_incarnations", Environment: collector.Environment, Cluster: collector.Cluster,
		CollectorInventorySHA256: collector.InventorySHA256}
	if len(collector.Handoffs) > 0 {
		result.Scope = "all_collected_member_generations"
	}
	seen := make(map[snapshotChainKey]bool, len(reports))
	verified := make(map[snapshotChainKey]replication.TransitionSnapshotReport, len(reports))
	for _, evidence := range reports {
		member := evidence.Report
		key := snapshotChainKey{NodeID: member.NodeID, Incarnation: member.Incarnation, JournalID: member.JournalID}
		chain, ok := chains[key]
		if !validSnapshotMember(collector, member) || !ok || seen[key] {
			return snapshotHeadComparison{}, errors.New("member snapshot has missing, duplicate or invalid chain identity")
		}
		seen[key] = true
		verified[key] = member
		match, err := compareOneSnapshotChain(chain, evidence)
		if err != nil {
			return snapshotHeadComparison{}, err
		}
		result.Members = append(result.Members, match)
	}
	if len(seen) != len(chains) {
		return snapshotHeadComparison{}, errors.New("member snapshots do not cover every collected chain")
	}
	for _, handoff := range collector.Handoffs {
		oldMember, oldOK := verified[snapshotChainKey{NodeID: handoff.NodeID, Incarnation: handoff.Incarnation, JournalID: handoff.OldJournalID}]
		newMember, newOK := verified[snapshotChainKey{NodeID: handoff.NodeID, Incarnation: handoff.Incarnation, JournalID: handoff.NewJournalID}]
		if !oldOK || !newOK || oldMember.InventorySHA256 != handoff.OldMemberInventorySHA256 ||
			oldMember.EventsSHA256 != handoff.OldEventsSHA256 ||
			newMember.InventorySHA256 != handoff.NewMemberInventorySHA256 ||
			newMember.BaselineDigest != handoff.NewBaselineDigest {
			return snapshotHeadComparison{}, errors.New("archived member snapshots do not match the recorded generation handoff")
		}
	}
	slices.SortFunc(result.Members, func(a, b snapshotHeadMatch) int {
		if a.NodeID != b.NodeID {
			return strings.Compare(a.NodeID, b.NodeID)
		}
		if a.Incarnation != b.Incarnation {
			return strings.Compare(a.Incarnation, b.Incarnation)
		}
		return strings.Compare(a.JournalID, b.JournalID)
	})
	return result, nil
}
