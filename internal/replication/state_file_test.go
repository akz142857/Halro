package replication

import (
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
