package app

import (
	"os"
	"path/filepath"
	"testing"
)

// Board §42.1 finding 1: a restore killed after extraction leaves its stage,
// a full copy of the restored data, beside the data directory. A stage is
// reclaimed only when its owner no longer holds its lock and its record names
// this data directory; a live, foreign or unrecorded stage is kept.
func TestRestoreStageReclaimKeepsLiveForeignAndUnrecordedStages(t *testing.T) {
	parent := t.TempDir()
	data := filepath.Join(parent, "data")
	stage := func(name string) string {
		path := filepath.Join(parent, name)
		if err := os.MkdirAll(filepath.Join(path, "extract", "data"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "extract", "data", "ledger.wal"), []byte("copy"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	exited, live, foreign, unrecorded := stage(".halro-restore-stage-1"), stage(".halro-restore-stage-2"), stage(".halro-restore-stage-3"), stage(".halro-restore-stage-4")
	f, err := holdRestoreStage(exited, data)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil { // the owner process exited
		t.Fatal(err)
	}
	held, err := holdRestoreStage(live, data)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	other, err := holdRestoreStage(foreign, filepath.Join(parent, "other-data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unrecorded, restoreStageOwnerName), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := reclaimRestoreStages(parent, data)
	if err != nil || removed != 1 {
		t.Fatal("reclaim", removed, err)
	}
	if _, err := os.Lstat(exited); !os.IsNotExist(err) {
		t.Fatal("exited owner's stage was kept", err)
	}
	for _, path := range []string{live, foreign, unrecorded} {
		if _, err := os.Stat(filepath.Join(path, "extract", "data", "ledger.wal")); err != nil {
			t.Fatal("reclaim removed a stage it cannot prove dead", path, err)
		}
	}
	// Retained debt keeps its lock for the life of the process.
	if err := releaseRestoreStage(held, false); err != nil {
		t.Fatal(err)
	}
	if removed, err := reclaimRestoreStages(parent, data); err != nil || removed != 0 {
		t.Fatal("retained debt's stage was reclaimed", removed, err)
	}
}
