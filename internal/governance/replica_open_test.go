package governance

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReplicaOpenRequiresSeededGovernanceJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "governance.journal")
	log, err := OpenWithOptions(path, make([]byte, governanceHMACKeySize), Options{Replica: true})
	if log != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replica open log=%v err=%v", log, err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Replica open created a seed directory: %v", err)
	}
}

func TestReplicaOpenDoesNotRepairGovernanceTailBeforeOrderingReconciliation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "governance.journal")
	if err := os.WriteFile(path, []byte{1}, 0o600); err != nil {
		t.Fatal(err)
	}
	if log, err := OpenWithOptions(path, make([]byte, governanceHMACKeySize), Options{Replica: true}); log != nil || err == nil {
		t.Fatalf("partial Replica governance log=%v err=%v", log, err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 1 {
		t.Fatalf("Replica open repaired the tail: size=%v err=%v", info, err)
	}
	if err := RepairReplicaTail(path, make([]byte, governanceHMACKeySize), 0); err != nil {
		t.Fatal(err)
	}
	log, err := OpenWithOptions(path, make([]byte, governanceHMACKeySize), Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}
