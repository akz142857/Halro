package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSelectedSeedOpaqueControlCatalogRefusesUnknownSlotsOversizeAndDirectories(t *testing.T) {
	for _, c := range []struct {
		name      string
		bytes     int
		directory bool
	}{
		{"cluster/state.json.received.json", 8193, false},
		{"cluster/state.json.received.json.unqualified-3", 8193, false},
		{"cluster/state.json.received.json.unqualified-4", 1, false},
		{"cluster/state.json.received.json.unqualified-00", 1, false},
		{"ha-group-key-history", haGroupHistoryLimit + 1, false},
		{"cluster/state.json.received.json", 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := testConfig(t)
			limits := selectedRuntimeBudgetFixture()
			cfg.ResourceBudget = &limits
			cfg.Storage.DataDir = t.TempDir()
			path := filepath.Join(cfg.Storage.DataDir, c.name)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if c.directory {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, make([]byte, c.bytes), 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			manager := functionalHAMetadataManager(t, cfg)
			paid := manager.Snapshot().Used
			_, err = hashSelectedSeedTree(context.Background(), cfg, cfg.Storage.DataDir, t.TempDir(), manager)
			if !errors.Is(err, errSeedMemoryStateAuthority) {
				t.Fatal("unqualified control entered approved tree", err)
			}
			after, statErr := os.Lstat(path)
			if statErr != nil || !os.SameFile(before, after) || before.Size() != after.Size() || manager.Snapshot().Used != paid {
				t.Fatal("rejected source control changed or lost fees", statErr, manager.Snapshot())
			}
		})
	}
}
