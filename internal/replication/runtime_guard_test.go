package replication

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnavailableRuntimeGuardKeepsClusterStateOutOfStandalone(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(filepath.Join(dataDir, ClusterDirectoryName), 0o700); err != nil {
		t.Fatal(err)
	}
	err := GuardUnavailableRuntime(dataDir, false, "serve")
	if err == nil || !strings.Contains(err.Error(), "cannot downgrade") || !strings.Contains(err.Error(), "refusing serve") {
		t.Fatalf("missing downgrade refusal: %v", err)
	}
}

func TestUnavailableRuntimeGuardDistinguishesStandaloneJoinAndMember(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := GuardUnavailableRuntime(dataDir, false, "serve"); err != nil {
		t.Fatalf("fresh Standalone refused: %v", err)
	}
	if err := GuardUnavailableRuntime(dataDir, true, "serve"); err == nil || !strings.Contains(err.Error(), "join or re-seed") {
		t.Fatalf("configured member without state error=%v", err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, ClusterDirectoryName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := GuardUnavailableRuntime(dataDir, true, "serve"); err == nil || !strings.Contains(err.Error(), "outside the replication order") {
		t.Fatalf("configured member with state error=%v", err)
	}
}

func TestUnavailableRuntimeGuardRefusesNonDirectorySentinel(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, ClusterDirectoryName), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := GuardUnavailableRuntime(dataDir, false, "init"); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("non-directory sentinel error=%v", err)
	}
}

func TestRequireMemberRuntimeAcceptsOnlyConfiguredSeededMember(t *testing.T) {
	root := t.TempDir()
	if err := RequireMemberRuntime(root, true, "start"); err == nil || !strings.Contains(err.Error(), "member state is absent") {
		t.Fatalf("missing cluster error=%v", err)
	}
	cluster := filepath.Join(root, ClusterDirectoryName)
	if err := os.Mkdir(cluster, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RequireMemberRuntime(root, true, "start"); err == nil || !strings.Contains(err.Error(), "authenticated member state is absent") {
		t.Fatalf("missing state error=%v", err)
	}
	state := filepath.Join(cluster, "state.json")
	if err := os.WriteFile(state, []byte("seeded"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RequireMemberRuntime(root, true, "start"); err != nil {
		t.Fatalf("seeded configured member rejected: %v", err)
	}
	if err := RequireMemberRuntime(root, false, "start"); err == nil || !strings.Contains(err.Error(), "cannot downgrade") {
		t.Fatalf("unconfigured member error=%v", err)
	}
}
