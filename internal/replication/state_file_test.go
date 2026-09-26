package replication

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestStateFilePublishesAndAuthenticates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster", "state.json")
	key := []byte("0123456789abcdef0123456789abcdef")
	state := validMemberState()
	if err := WriteState(path, state, key); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode=%#o, want 0600", info.Mode().Perm())
	}
	decoded, err := ReadState(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ClusterID != state.ClusterID || decoded.DurableIndex != state.DurableIndex {
		t.Fatalf("decoded=%#v", decoded)
	}

	state.Role = RoleReplica
	state.Term++
	state.PromisedTerm++
	if err := WriteState(path, state, key); err != nil {
		t.Fatal(err)
	}
	decoded, err = ReadState(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Role != RoleReplica || decoded.Term != state.Term {
		t.Fatalf("replacement decoded=%#v", decoded)
	}
}

func TestStateBootstrapIsBoundedAndMustBeFollowedByAuthentication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	masterKey := bytes.Repeat([]byte{0x42}, 32)
	clusterKey, err := DeriveClusterKey(masterKey, "inc_01")
	if err != nil {
		t.Fatal(err)
	}
	state := validMemberState()
	if err := WriteState(path, state, clusterKey[:]); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := ReadStateBootstrap(path)
	if err != nil || bootstrap.Incarnation != state.Incarnation {
		t.Fatalf("bootstrap=%#v err=%v", bootstrap, err)
	}
	decoded, err := ReadStateWithMasterKey(path, masterKey)
	if err != nil || decoded.NodeID != state.NodeID || decoded.ConfirmedIndex != state.ConfirmedIndex {
		t.Fatalf("decoded=%#v err=%v", decoded, err)
	}

	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(encoded, []byte(`"node_id":"halro-0"`), []byte(`"node_id":"halro-9"`), 1)
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStateBootstrap(path); err != nil {
		t.Fatalf("bootstrap should only supply the key salt: %v", err)
	}
	if _, err := ReadStateWithMasterKey(path, masterKey); err == nil {
		t.Fatal("unauthenticated bootstrap fields were accepted as member authority")
	}

	oversized := append([]byte(`{"version":2,"incarnation":"inc_01"}`), bytes.Repeat([]byte(" "), MaxMemberStateJSON)...)
	if err := os.WriteFile(path, oversized, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStateBootstrap(path); err == nil {
		t.Fatal("oversized bootstrap was accepted")
	}
}
