package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/app"
	halroconfig "github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/vault"
	"gopkg.in/yaml.v3"
)

func TestVerifyMemberSnapshotHeadsAuthenticatesFrozenFiles(t *testing.T) {
	root := t.TempDir()
	nodes := []string{"node-1", "node-2"}
	entries := make([]directMemberSnapshot, 0, len(nodes))
	reports := make(map[string]replication.TransitionSnapshotReport, len(nodes))
	pages := make(map[string]durableTransitionPage, len(nodes))
	for index, node := range nodes {
		other := nodes[1-index]
		memberRoot := filepath.Join(root, node)
		snapshotDir := filepath.Join(memberRoot, "snapshot")
		if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
			t.Fatal(err)
		}
		masterPath := filepath.Join(memberRoot, "master.key")
		if err := vault.InitMasterKey(masterPath); err != nil {
			t.Fatal(err)
		}
		master, err := vault.LoadMasterKey(masterPath)
		if err != nil {
			t.Fatal(err)
		}
		key, err := replication.DeriveClusterKey(master, "inc_1")
		clear(master)
		if err != nil {
			t.Fatal(err)
		}
		listen := "127.0.0.1:9910"
		peerAddress := "127.0.0.1:9911"
		if index == 1 {
			listen, peerAddress = peerAddress, listen
		}
		pin := "sha256:" + strings.Repeat("a", 64)
		cfg := halroconfig.Default()
		cfg.Storage.DataDir = filepath.Join(memberRoot, "original-not-mounted")
		cfg.Storage.MasterKey = halroconfig.MasterKey{Mode: halroconfig.MasterKeyModeFile, File: masterPath}
		cfg.Replication = &halroconfig.Replication{ClusterID: "ha", NodeID: node, Listen: listen,
			Peers: []halroconfig.ReplicationPeer{{Name: other, Address: peerAddress, SPKISHA256: pin}},
			TLS:   halroconfig.ReplicationTLS{CAFile: "/private/ca.crt", CertFile: "/private/tls.crt", KeyFile: "/private/tls.key"}}
		state := replication.MemberState{Version: replication.StateVersion, ClusterID: "ha", Incarnation: "inc_1", NodeID: node,
			Role: replication.RoleReplica, Term: 1, PromisedTerm: 1,
			Peers: []replication.StatePeer{{Name: other, Address: peerAddress, SPKISHA256: pin}}}
		if index == 0 {
			state.Role = replication.RolePrimary
		}
		statePath := filepath.Join(snapshotDir, "cluster", "state.json")
		if err := replication.WriteState(statePath, state, key[:]); err != nil {
			t.Fatal(err)
		}
		cursor, err := replication.CreateTransitionJournal(replication.TransitionJournalPath(statePath), key[:], state, "initial_state")
		if err != nil {
			t.Fatal(err)
		}
		state.Version, state.Transition = replication.TransitionStateVersion, cursor
		if err := replication.WriteState(statePath, state, key[:]); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			publisher, err := replication.NewDurableStatePublisher(statePath, key[:], state)
			if err != nil {
				t.Fatal(err)
			}
			state, err = publisher.Promise(2)
			publisher.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		journal, err := replication.OpenTransitionJournal(replication.TransitionJournalPath(statePath), key[:], state)
		if err != nil {
			t.Fatal(err)
		}
		memberPage, err := journal.Page(0, "", 64)
		if err != nil {
			t.Fatal(err)
		}
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
		pageBytes, err := json.Marshal(memberPage)
		if err != nil {
			t.Fatal(err)
		}
		var page durableTransitionPage
		if err := json.Unmarshal(pageBytes, &page); err != nil {
			t.Fatal(err)
		}
		pages[node] = page
		clear(key[:])
		configBytes, err := yaml.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		configPath := filepath.Join(memberRoot, "config.yaml")
		if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		report, err := app.VerifyMemberTransitionSnapshot(context.Background(), cfg, snapshotDir)
		if err != nil {
			t.Fatal(err)
		}
		reports[node] = report
		entries = append(entries, directMemberSnapshot{NodeID: node, Config: configPath, SnapshotDir: snapshotDir})
	}
	archivePath := filepath.Join(root, "collector", "durable.json")
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o700); err != nil {
		t.Fatal(err)
	}
	archive, err := openDurableArchive(archivePath, "test", "ha", nodes)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		report := reports[node]
		role := "replica"
		if node == "node-1" {
			role = "primary"
		}
		page := pages[node]
		cursor := durableTransitionCursor{JournalID: report.JournalID, Sequence: page.NextCursor.Sequence, Digest: page.NextCursor.Digest,
			BaselineDigest: report.BaselineDigest, Role: role, Term: 1, PromisedTerm: 1}
		if len(page.Events) > 0 {
			last := page.Events[len(page.Events)-1]
			cursor.Role, cursor.Term, cursor.PromisedTerm = last.ToRole, last.ToTerm, last.ToPromisedTerm
		}
		if !archive.capture(node, page, cursor, time.Now().UTC()) {
			t.Fatalf("collector could not retain baseline for %s", node)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	collector, err := verifyDurableSnapshot(archivePath, "test", "ha", nodes)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, "members.json")
	manifest, err := json.Marshal(map[string]any{"version": 1, "members": entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	comparison, err := verifyMemberSnapshotHeads(context.Background(), collector, manifestPath)
	if err != nil || comparison.Status != "member_authenticated_current_chain_match" || len(comparison.Members) != 2 ||
		len(comparison.VerifiedMemberReports) != 2 || comparison.Members[0].MemberInventorySHA256 != reports["node-1"].InventorySHA256 ||
		comparison.Members[0].EventsSHA256 != reports["node-1"].EventsSHA256 ||
		reports["node-1"].EventsSHA256 == reports["node-2"].EventsSHA256 {
		t.Fatalf("direct member verification=%+v err=%v", comparison, err)
	}
	oldDir := filepath.Join(root, "node-1-old", "snapshot")
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	master, err := vault.LoadMasterKey(filepath.Join(root, "node-1", "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	oldKey, err := replication.DeriveClusterKey(master, "inc_0")
	clear(master)
	if err != nil {
		t.Fatal(err)
	}
	oldState := replication.MemberState{Version: replication.StateVersion, ClusterID: "ha", Incarnation: "inc_0",
		NodeID: "node-1", Role: replication.RoleReplica, Term: 1, PromisedTerm: 1,
		Peers: []replication.StatePeer{{Name: "node-2", Address: "127.0.0.1:9911", SPKISHA256: "sha256:" + strings.Repeat("a", 64)}}}
	oldStatePath := filepath.Join(oldDir, "cluster", "state.json")
	if err := replication.WriteState(oldStatePath, oldState, oldKey[:]); err != nil {
		t.Fatal(err)
	}
	oldCursor, err := replication.CreateTransitionJournal(replication.TransitionJournalPath(oldStatePath), oldKey[:], oldState, "initial_state")
	if err != nil {
		t.Fatal(err)
	}
	oldState.Version, oldState.Transition = replication.TransitionStateVersion, oldCursor
	if err := replication.WriteState(oldStatePath, oldState, oldKey[:]); err != nil {
		t.Fatal(err)
	}
	oldJournal, err := replication.OpenTransitionJournal(replication.TransitionJournalPath(oldStatePath), oldKey[:], oldState)
	if err != nil {
		t.Fatal(err)
	}
	oldMemberPage, err := oldJournal.Page(0, "", 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := oldJournal.Close(); err != nil {
		t.Fatal(err)
	}
	clear(oldKey[:])
	oldPageBytes, err := json.Marshal(oldMemberPage)
	if err != nil {
		t.Fatal(err)
	}
	var oldPage durableTransitionPage
	if err := json.Unmarshal(oldPageBytes, &oldPage); err != nil {
		t.Fatal(err)
	}
	historicalPath := filepath.Join(root, "collector-history", "durable.json")
	if err := os.MkdirAll(filepath.Dir(historicalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	historicalArchive, err := openDurableArchive(historicalPath, "test", "ha", nodes)
	if err != nil {
		t.Fatal(err)
	}
	oldCollectorCursor := durableTransitionCursor{JournalID: oldPage.NextCursor.JournalID, Digest: oldPage.NextCursor.Digest,
		BaselineDigest: oldPage.BaselineDigest, Role: oldPage.BaselineRole, Term: oldPage.BaselineTerm, PromisedTerm: oldPage.BaselinePromised}
	if !historicalArchive.capture("node-1", oldPage, oldCollectorCursor, time.Now().UTC()) {
		t.Fatal("collector could not retain historical incarnation")
	}
	for _, node := range nodes {
		page := pages[node]
		cursor := durableTransitionCursor{JournalID: page.NextCursor.JournalID, Sequence: page.NextCursor.Sequence,
			Digest: page.NextCursor.Digest, BaselineDigest: page.BaselineDigest,
			Role: page.BaselineRole, Term: page.BaselineTerm, PromisedTerm: page.BaselinePromised}
		if len(page.Events) > 0 {
			last := page.Events[len(page.Events)-1]
			cursor.Role, cursor.Term, cursor.PromisedTerm = last.ToRole, last.ToTerm, last.ToPromisedTerm
		}
		if !historicalArchive.capture(node, page, cursor, time.Now().UTC()) {
			t.Fatalf("collector could not retain current incarnation for %s", node)
		}
	}
	if err := historicalArchive.Close(); err != nil {
		t.Fatal(err)
	}
	historicalCollector, err := verifyDurableSnapshot(historicalPath, "test", "ha", nodes)
	if err != nil {
		t.Fatal(err)
	}
	allEntries := []directMemberSnapshot{{NodeID: "node-1", Incarnation: "inc_0", Config: entries[0].Config, SnapshotDir: oldDir}}
	for _, entry := range entries {
		entry.Incarnation = "inc_1"
		allEntries = append(allEntries, entry)
	}
	allManifestPath := filepath.Join(root, "all-members.json")
	writeAllManifest := func(members []directMemberSnapshot) {
		t.Helper()
		data, err := json.Marshal(map[string]any{"version": 2, "members": members})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(allManifestPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeAllManifest(allEntries)
	allComparison, err := verifyMemberSnapshotHeads(context.Background(), historicalCollector, allManifestPath)
	if err != nil || allComparison.Status != "member_authenticated_all_collected_chains_match" ||
		allComparison.Scope != "all_collected_incarnations" || len(allComparison.Members) != 3 ||
		allComparison.Members[0].Incarnation != "inc_0" {
		t.Fatalf("all-chain verification=%+v err=%v", allComparison, err)
	}
	writeAllManifest(allEntries[1:])
	if _, err := verifyMemberSnapshotHeads(context.Background(), historicalCollector, allManifestPath); err == nil {
		t.Fatal("all-chain verification accepted a missing historical snapshot")
	}
	wrongIncarnation := append([]directMemberSnapshot(nil), allEntries...)
	wrongIncarnation[0].Incarnation = "inc-other"
	writeAllManifest(wrongIncarnation)
	if _, err := verifyMemberSnapshotHeads(context.Background(), historicalCollector, allManifestPath); err == nil {
		t.Fatal("all-chain verification accepted a wrong historical incarnation")
	}
	repeatedDirectory := append([]directMemberSnapshot(nil), allEntries...)
	repeatedDirectory[0].SnapshotDir = repeatedDirectory[1].SnapshotDir
	writeAllManifest(repeatedDirectory)
	if _, err := verifyMemberSnapshotHeads(context.Background(), historicalCollector, allManifestPath); err == nil {
		t.Fatal("all-chain verification accepted a reused frozen directory")
	}
	writeAllManifest(allEntries)
	laggingCollector := historicalCollector
	laggingCollector.Chains = append([]durableSnapshotChain(nil), historicalCollector.Chains...)
	laggingCollector.Chains[0].Coverage = "catching_up"
	if _, err := verifyMemberSnapshotHeads(context.Background(), laggingCollector, allManifestPath); err == nil {
		t.Fatal("all-chain verification accepted a collector chain that was still catching up")
	}
	// A structurally valid collector file can retain the opaque MAC digest
	// while changing an event field. The member-derived event root must catch it.
	collectorBytes, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	var changed durableArchiveDocument
	if err := json.Unmarshal(collectorBytes, &changed); err != nil {
		t.Fatal(err)
	}
	chain := changed.Chains["node-1"][0]
	chain.Events[0].At = chain.Events[0].At.Add(time.Nanosecond)
	chain.LastEventAt = chain.Events[0].At
	changed.Chains["node-1"][0] = chain
	changedBytes, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivePath, changedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	changedCollector, err := verifyDurableSnapshot(archivePath, "test", "ha", nodes)
	if err != nil {
		t.Fatalf("test mutation should retain collector-local structure: %v", err)
	}
	if _, err := verifyMemberSnapshotHeads(context.Background(), changedCollector, manifestPath); err == nil {
		t.Fatal("direct handoff accepted a changed collector event with the same head digest")
	}
	if err := os.WriteFile(archivePath, collectorBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	writeManifest := func() {
		t.Helper()
		data, err := json.Marshal(map[string]any{"version": 1, "members": entries})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entries[0].NodeID = entries[1].NodeID
	writeManifest()
	if _, err := verifyMemberSnapshotHeads(context.Background(), collector, manifestPath); err == nil {
		t.Fatal("direct handoff accepted a duplicate member")
	}
	entries[0].NodeID = nodes[0]
	entries[0].Config = entries[1].Config
	writeManifest()
	if _, err := verifyMemberSnapshotHeads(context.Background(), collector, manifestPath); err == nil {
		t.Fatal("direct handoff accepted another member's configuration")
	}
	entries[0].Config = filepath.Join(root, nodes[0], "config.yaml")
	writeManifest()
	journalPath := filepath.Join(entries[0].SnapshotDir, "cluster", "transitions.journal")
	journal, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	journal[20] ^= 1
	if err := os.WriteFile(journalPath, journal, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyMemberSnapshotHeads(context.Background(), collector, manifestPath); err == nil {
		t.Fatal("direct handoff accepted a tampered member journal")
	}
}
