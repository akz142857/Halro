package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultIsValidAndLoopbackOnly(t *testing.T) {
	cfg := Default()
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if cfg.Server.GatewayListen != "127.0.0.1:8080" || cfg.Server.AdminListen != "127.0.0.1:8081" {
		t.Fatalf("unsafe default listeners: %#v", cfg.Server)
	}
}

func TestWriteDefaultDoesNotReplaceExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	if err := WriteDefault(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%#o", info.Mode().Perm())
	}
	cfg, err := Load(path, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != SchemaVersion || cfg.Storage.MetadataFile != "halro.db" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteDefault(path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second write error=%v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("existing config was replaced")
	}
}

// TestDefaultAttemptBudgetReachesAFanOut states the relation between the two
// attempt ceilings as a property, because the numbers on their own do not show
// it and getting it wrong is silent.
//
// The budget is shared across the whole request. max_attempts_per_target decides
// how much of it one target may spend before the loop moves on, so the number of
// distinct candidates a request can reach is
// ceil(max_total_attempts / max_attempts_per_target) — not the number of routes
// configured, and it is a floor: a target refused before dispatch (an open
// breaker, a full concurrency gate) spends no budget, so later targets can still
// be reached. Shipping 3 and 2 put that floor at two, which meant a third route
// could be configured, enabled and healthy and still never called.
func TestDefaultAttemptBudgetReachesAFanOut(t *testing.T) {
	cfg := Default()
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	total, perTarget := cfg.Gateway.MaxTotalAttempts, cfg.Retry.MaxAttemptsPerTarget
	if perTarget < 1 {
		t.Fatalf("retry.max_attempts_per_target=%d", perTarget)
	}
	reachable := (total + perTarget - 1) / perTarget
	// Four rather than two: a default that cannot walk a modest fan-out turns
	// configuration into decoration, and the operator has no signal that it did.
	if reachable < 4 {
		t.Fatalf(
			"the default attempt budget reaches %d candidates (max_total_attempts=%d, max_attempts_per_target=%d); "+
				"a route beyond that is configured and never called",
			reachable, total, perTarget,
		)
	}
}
