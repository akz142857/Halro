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
	if cfg.Version != SchemaVersion || cfg.Storage.MetadataFile != "heimdall.db" {
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
