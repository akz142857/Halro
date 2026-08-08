package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/config"
)

func TestInitializeIfNeededIsIdempotentAndFailClosed(t *testing.T) {
	cfg := testConfig(t)
	state, err := InspectInitialization(cfg)
	if err != nil || state != InitializationEmpty {
		t.Fatalf("initial state=%q err=%v", state, err)
	}
	created, err := InitializeIfNeeded(cfg)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	created, err = InitializeIfNeeded(cfg)
	if err != nil || created {
		t.Fatalf("second created=%v err=%v", created, err)
	}
	before, err := os.ReadFile(cfg.Storage.MasterKey.File)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(cfg.Storage.MasterKey.File)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("idempotent initialization replaced the master key")
	}

	partial := testConfig(t)
	if err := os.MkdirAll(filepath.Dir(partial.Storage.MasterKey.File), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial.Storage.MasterKey.File, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if state, err := InspectInitialization(partial); err != nil || state != InitializationInconsistent {
		t.Fatalf("partial state=%q err=%v", state, err)
	}
	if _, err := InitializeIfNeeded(partial); err == nil {
		t.Fatal("partial initialization was not rejected")
	}
}

func TestInspectInitializationRejectsOrphanedData(t *testing.T) {
	cfg := testConfig(t)
	if err := os.MkdirAll(cfg.Storage.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Storage.DataDir, "orphan"), []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := InspectInitialization(cfg)
	if err != nil || state != InitializationInconsistent {
		t.Fatalf("state=%q err=%v", state, err)
	}
}

func TestInspectInitializationRejectsMissingDurableFiles(t *testing.T) {
	for _, missing := range []struct {
		name string
		path func(config.Config) string
	}{
		{name: "ledger", path: func(cfg config.Config) string { return cfg.LedgerPath() }},
		{name: "audit", path: func(cfg config.Config) string { return cfg.AuditPath() }},
	} {
		t.Run(missing.name, func(t *testing.T) {
			cfg := testConfig(t)
			if err := Initialize(cfg); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(missing.path(cfg)); err != nil {
				t.Fatal(err)
			}
			state, err := InspectInitialization(cfg)
			if err != nil || state != InitializationInconsistent {
				t.Fatalf("state=%q err=%v", state, err)
			}
			if _, err := InitializeIfNeeded(cfg); err == nil {
				t.Fatal("incomplete durable state was not rejected")
			}
		})
	}
}
