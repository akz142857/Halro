package ledger

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReplicaOpenRequiresSeededLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "ledger.wal")
	log, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: make([]byte, 32), Replica: true})
	if log != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replica open log=%v err=%v", log, err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Replica open created a seed directory: %v", err)
	}
}

func TestReplicaOpenDoesNotRepairLedgerTailBeforeOrderingReconciliation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	if err := os.WriteFile(path, []byte{1}, 0o600); err != nil {
		t.Fatal(err)
	}
	status := NewStatus()
	if log, err := OpenWithOptions(path, status, Options{ChainKey: make([]byte, 32), Replica: true}); log != nil || err == nil {
		t.Fatalf("partial Replica Ledger=%v err=%v", log, err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 1 {
		t.Fatalf("Replica open repaired the tail: size=%v err=%v", info, err)
	}
	if err := RepairReplicaTail(path, make([]byte, 32), 1, 0); err != nil {
		t.Fatal(err)
	}
	log, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: make([]byte, 32), Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}
