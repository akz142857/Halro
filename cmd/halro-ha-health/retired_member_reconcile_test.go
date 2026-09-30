package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	halroconfig "github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/vault"
)

func TestReconcileRetiredMemberImportsOnlyAuthenticatedOldTail(t *testing.T) {
	root := t.TempDir()
	snapshotDir := filepath.Join(root, "retired")
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		t.Fatal(err)
	}
	masterPath := filepath.Join(root, "master.key")
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
	pin := "sha256:" + strings.Repeat("a", 64)
	cfg := halroconfig.Default()
	cfg.Storage.DataDir = filepath.Join(root, "not-mounted")
	cfg.Storage.MasterKey = halroconfig.MasterKey{Mode: halroconfig.MasterKeyModeFile, File: masterPath}
	cfg.Replication = &halroconfig.Replication{ClusterID: "ha", NodeID: "node-1", Listen: "127.0.0.1:9910",
		Peers: []halroconfig.ReplicationPeer{{Name: "node-2", Address: "127.0.0.1:9911", SPKISHA256: pin}},
		TLS:   halroconfig.ReplicationTLS{CAFile: "/private/ca.crt", CertFile: "/private/tls.crt", KeyFile: "/private/tls.key"}}
	state := replication.MemberState{Version: replication.StateVersion, ClusterID: "ha", Incarnation: "inc_1", NodeID: "node-1",
		Role: replication.RolePrimary, Term: 1, PromisedTerm: 1,
		Peers: []replication.StatePeer{{Name: "node-2", Address: "127.0.0.1:9911", SPKISHA256: pin}}}
	statePath := filepath.Join(snapshotDir, "cluster", "state.json")
	if err := replication.WriteState(statePath, state, key[:]); err != nil {
		t.Fatal(err)
	}
	baseline, err := replication.CreateTransitionJournal(replication.TransitionJournalPath(statePath), key[:], state, "initial_state")
	if err != nil {
		t.Fatal(err)
	}
	state.Version, state.Transition = replication.TransitionStateVersion, baseline
	if err := replication.WriteState(statePath, state, key[:]); err != nil {
		t.Fatal(err)
	}
	journal, err := replication.OpenTransitionJournal(replication.TransitionJournalPath(statePath), key[:], state)
	if err != nil {
		t.Fatal(err)
	}
	memberBaseline, err := journal.Page(0, "", 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(memberBaseline)
	if err != nil {
		t.Fatal(err)
	}
	var first durableTransitionPage
	if err := decodeExactJSON(encoded, &first); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(root, "collector", "durable.json")
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o700); err != nil {
		t.Fatal(err)
	}
	members := []string{"node-1", "node-2"}
	archive, err := openDurableArchive(archivePath, "test", "ha", members)
	if err != nil {
		t.Fatal(err)
	}
	firstCursor := durableTransitionCursor{JournalID: first.NextCursor.JournalID, Digest: first.NextCursor.Digest,
		BaselineDigest: first.BaselineDigest, Role: first.BaselineRole, Term: first.BaselineTerm, PromisedTerm: first.BaselinePromised}
	if !archive.capture("node-1", first, firstCursor, time.Now().UTC()) {
		t.Fatal("could not store pre-fault baseline")
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	publisher, err := replication.NewDurableStatePublisher(statePath, key[:], state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Promise(2); err != nil {
		t.Fatal(err)
	}
	publisher.Close()
	clear(key[:])
	result, err := reconcileRetiredMemberSnapshot(context.Background(), archivePath, "test", "ha", members, cfg, snapshotDir)
	if err != nil || result.Status != "retired_member_tail_imported_new_generation_unapproved" ||
		result.FromSequence != 0 || result.ImportedEvents != 1 || result.CommittedSequence != 1 || result.MemberInventorySHA256 == "" {
		t.Fatalf("retired member reconciliation=%+v err=%v", result, err)
	}
	archive, err = openDurableArchive(archivePath, "test", "ha", members)
	if err != nil {
		t.Fatal(err)
	}
	stored := archive.cursor("node-1", "inc_1")
	if stored == nil || stored.Sequence != 1 || stored.Digest != result.CommittedDigest || len(archive.doc.Chains["node-1"]) != 1 {
		t.Fatalf("collector did not retain exactly the old chain tail: %+v", stored)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := reconcileRetiredMemberSnapshot(context.Background(), archivePath, "test", "ha", members, cfg, snapshotDir)
	if err != nil || again.ImportedEvents != 0 || again.FromSequence != 1 {
		t.Fatalf("idempotent old tail reconciliation=%+v err=%v", again, err)
	}
	journalPath := replication.TransitionJournalPath(statePath)
	bytes, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	bytes[20] ^= 1
	if err := os.WriteFile(journalPath, bytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileRetiredMemberSnapshot(context.Background(), archivePath, "test", "ha", members, cfg, snapshotDir); err == nil {
		t.Fatal("tampered retired member journal accepted")
	}
}
