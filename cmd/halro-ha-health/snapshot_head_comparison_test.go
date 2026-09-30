package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/replication"
)

func TestCompareMemberSnapshotReportsRequiresExactCurrentHeads(t *testing.T) {
	root := t.TempDir()
	write := func(name string, value any) string {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	collector := durableSnapshotReport{Version: 1, Status: "local_files_verified", Environment: "test", Cluster: "ha",
		Members: []string{"node-1", "node-2"}, InventorySHA256: strings.Repeat("f", 64),
		Chains: []durableSnapshotChain{
			{NodeID: "node-1", Incarnation: "inc_1", BaselineKind: "legacy_baseline", JournalID: "0123456789abcdef0123456789abcdef",
				BaselineDigest: strings.Repeat("a", 64), StoredSequence: 2, CommittedSequence: 2,
				StoredDigest: strings.Repeat("b", 64), HeadDigest: strings.Repeat("b", 64), EventsSHA256: strings.Repeat("e", 64), ObservedAt: time.Now().UTC(), Coverage: "caught_up_to_observed_head"},
			{NodeID: "node-2", Incarnation: "inc_1", BaselineKind: "initial_state", JournalID: "fedcba9876543210fedcba9876543210",
				BaselineDigest: strings.Repeat("c", 64), StoredSequence: 1, CommittedSequence: 1,
				StoredDigest: strings.Repeat("d", 64), HeadDigest: strings.Repeat("d", 64), EventsSHA256: strings.Repeat("e", 64), ObservedAt: time.Now().UTC(), Coverage: "caught_up_to_observed_head"},
		}}
	member := func(chain durableSnapshotChain) replication.TransitionSnapshotReport {
		report := replication.TransitionSnapshotReport{Version: 1, Status: "member_mac_verified", ClusterID: "ha",
			Incarnation: chain.Incarnation, NodeID: chain.NodeID, JournalID: chain.JournalID,
			BaselineKind: chain.BaselineKind, BaselineDigest: chain.BaselineDigest,
			Committed: chain.CommittedSequence, CommittedDigest: chain.HeadDigest, EventsSHA256: chain.EventsSHA256, Segments: 1,
			StorageBytes: 30, Files: []replication.TransitionSnapshotFile{
				{Name: "state.json", Bytes: 10, SHA256: strings.Repeat("1", 64)},
				{Name: "transitions.journal", Bytes: 20, SHA256: strings.Repeat("2", 64)},
			}}
		rootHash := sha256.New()
		for _, file := range report.Files {
			_, _ = fmt.Fprintf(rootHash, "%s\x00%d\x00%s\n", file.Name, file.Bytes, file.SHA256)
		}
		report.InventorySHA256 = hex.EncodeToString(rootHash.Sum(nil))
		return report
	}
	first, second := member(collector.Chains[0]), member(collector.Chains[1])
	firstPath := write("node-1.json", first)
	secondPath := write("node-2.json", second)
	manifestPath := write("manifest.json", map[string]any{"version": 1, "reports": []string{secondPath, firstPath}})
	comparison, err := compareMemberSnapshotReports(collector, manifestPath)
	if err != nil || comparison.Status != "snapshot_heads_match" || len(comparison.Members) != 2 ||
		comparison.Members[0].NodeID != "node-1" || comparison.Members[1].NodeID != "node-2" ||
		comparison.Members[0].MemberReportSHA256 == "" || comparison.CollectorInventorySHA256 != collector.InventorySHA256 {
		t.Fatalf("head comparison=%+v err=%v", comparison, err)
	}
	all, err := compareAllSnapshotChains(collector, []memberSnapshotEvidence{{Report: first}, {Report: second}})
	if err != nil || all.Status != "member_authenticated_all_collected_chains_match" || len(all.Members) != 2 {
		t.Fatalf("all-chain comparison=%+v err=%v", all, err)
	}
	older := collector.Chains[0]
	older.Incarnation, older.JournalID = "inc_0", "00112233445566778899aabbccddeeff"
	older.CommittedSequence, older.StoredSequence = 0, 0
	older.HeadDigest, older.StoredDigest = older.BaselineDigest, older.BaselineDigest
	older.EventsSHA256 = hex.EncodeToString(sha256.New().Sum(nil))
	withHistory := collector
	withHistory.Chains = append([]durableSnapshotChain{older}, collector.Chains...)
	if _, err := compareAllSnapshotChains(withHistory, []memberSnapshotEvidence{{Report: first}, {Report: second}}); err == nil {
		t.Fatal("all-chain comparison accepted a missing historical member copy")
	}
	oldMember := member(older)
	all, err = compareAllSnapshotChains(withHistory, []memberSnapshotEvidence{{Report: first}, {Report: oldMember}, {Report: second}})
	if err != nil || len(all.Members) != 3 || all.Members[0].Incarnation != "inc_0" {
		t.Fatalf("historical-chain comparison=%+v err=%v", all, err)
	}
	replacement := collector.Chains[0]
	replacement.JournalID = strings.Repeat("9", 32)
	replacement.BaselineDigest = strings.Repeat("8", 64)
	replacement.StoredSequence, replacement.CommittedSequence = 0, 0
	replacement.StoredDigest, replacement.HeadDigest = replacement.BaselineDigest, replacement.BaselineDigest
	replacement.EventsSHA256 = hex.EncodeToString(sha256.New().Sum(nil))
	withReseed := collector
	withReseed.Chains = []durableSnapshotChain{collector.Chains[0], replacement, collector.Chains[1]}
	replacementMember := member(replacement)
	withReseed.Handoffs = []durableReseedHandoff{{NodeID: "node-1", Incarnation: "inc_1",
		OldJournalID: first.JournalID, OldEventsSHA256: first.EventsSHA256,
		OldMemberInventorySHA256: first.InventorySHA256, NewJournalID: replacement.JournalID,
		NewBaselineDigest: replacement.BaselineDigest, NewMemberInventorySHA256: replacementMember.InventorySHA256}}
	if _, err := compareAllSnapshotChains(withReseed, []memberSnapshotEvidence{{Report: first}, {Report: second}}); err == nil {
		t.Fatal("all-chain comparison accepted a missing same-incarnation generation")
	}
	all, err = compareAllSnapshotChains(withReseed, []memberSnapshotEvidence{{Report: first}, {Report: replacementMember}, {Report: second}})
	if err != nil || len(all.Members) != 3 || all.Scope != "all_collected_member_generations" ||
		all.Members[0].JournalID == all.Members[1].JournalID {
		t.Fatalf("same-incarnation generation comparison=%+v err=%v", all, err)
	}
	badHandoff := withReseed
	badHandoff.Handoffs = append([]durableReseedHandoff(nil), withReseed.Handoffs...)
	badHandoff.Handoffs[0].OldMemberInventorySHA256 = strings.Repeat("0", 64)
	if _, err := compareAllSnapshotChains(badHandoff, []memberSnapshotEvidence{{Report: first}, {Report: replacementMember}, {Report: second}}); err == nil {
		t.Fatal("all-chain comparison ignored the authenticated old member inventory in the handoff")
	}
	for _, changed := range []struct {
		name   string
		modify func(*replication.TransitionSnapshotReport)
	}{
		{"member ahead", func(report *replication.TransitionSnapshotReport) { report.Committed++ }},
		{"digest conflict", func(report *replication.TransitionSnapshotReport) { report.CommittedDigest = strings.Repeat("0", 64) }},
		{"event conflict", func(report *replication.TransitionSnapshotReport) { report.EventsSHA256 = strings.Repeat("0", 64) }},
		{"old incarnation", func(report *replication.TransitionSnapshotReport) { report.Incarnation = "old" }},
		{"duplicate member", func(report *replication.TransitionSnapshotReport) { report.NodeID = "node-2" }},
		{"inventory mismatch", func(report *replication.TransitionSnapshotReport) { report.Files[0].SHA256 = strings.Repeat("3", 64) }},
	} {
		t.Run(changed.name, func(t *testing.T) {
			bad := first
			bad.Files = append([]replication.TransitionSnapshotFile(nil), first.Files...)
			changed.modify(&bad)
			write("node-1.json", bad)
			if _, err := compareMemberSnapshotReports(collector, manifestPath); err == nil {
				t.Fatal("mismatched member report passed head comparison")
			}
		})
	}
	write("node-1.json", first)
	lagging := collector
	lagging.Chains = append([]durableSnapshotChain(nil), collector.Chains...)
	lagging.Chains[0].StoredSequence--
	lagging.Chains[0].Coverage = "catching_up"
	if _, err := compareMemberSnapshotReports(lagging, manifestPath); err == nil {
		t.Fatal("collector catching up to its own observed head passed comparison")
	}
	if err := os.Chmod(secondPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := compareMemberSnapshotReports(collector, manifestPath); err == nil {
		t.Fatal("non-private report was accepted")
	}
}

func TestCompareMemberSnapshotReportsRejectsUnverifiedCollectorAndManifest(t *testing.T) {
	collector := durableSnapshotReport{Version: 1, Status: "partial", Members: []string{"node-1", "node-2"}, InventorySHA256: strings.Repeat("f", 64)}
	if _, err := compareMemberSnapshotReports(collector, "/missing"); err == nil {
		t.Fatal("unverified collector report was accepted")
	}
	collector.Status = "local_files_verified"
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"reports":[],"unexpected":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := compareMemberSnapshotReports(collector, path); err == nil {
		t.Fatal("malformed comparison manifest was accepted")
	}
}
