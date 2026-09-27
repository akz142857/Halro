package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/config"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func releasedConfig(t *testing.T, version string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "internal", "config", "testdata", "releases", version+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestMigrateKeepsTheOriginalBesideTheMigratedFile is the promise that makes a
// --write safe to run before an upgrade: the way back is a file, not a memory.
func TestMigrateKeepsTheOriginalBesideTheMigratedFile(t *testing.T) {
	original := releasedConfig(t, "v0.8.5")
	path := writeConfig(t, original)

	if err := migrateConfigCommand(path, true); err != nil {
		t.Fatal(err)
	}
	kept, err := os.ReadFile(path + ".before-migrate")
	if err != nil {
		t.Fatal(err)
	}
	if string(kept) != original {
		t.Error("the kept copy is not what was there before")
	}
	if _, err := config.Load(path, config.LoadOptions{}); err != nil {
		t.Fatalf("the migrated file does not load: %v", err)
	}
}

// TestMigrateWillNotOverwriteAnEarlierRunsCopy stops a second run from
// destroying the only copy of the pre-migration file — which is exactly what an
// operator re-running the command after an unrelated failure would do.
func TestMigrateWillNotOverwriteAnEarlierRunsCopy(t *testing.T) {
	path := writeConfig(t, releasedConfig(t, "v0.8.5"))
	if err := os.WriteFile(path+".before-migrate", []byte("an earlier run kept this\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := migrateConfigCommand(path, true)
	if err == nil {
		t.Fatal("overwrote the copy an earlier run kept")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error does not say why: %v", err)
	}
	kept, readErr := os.ReadFile(path + ".before-migrate")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(kept) != "an earlier run kept this\n" {
		t.Error("the earlier copy was replaced anyway")
	}
}

// TestMigrateDryRunWritesNothing keeps the default harmless. It is the mode an
// error message tells an operator to run, so it must be the mode that cannot
// cost them anything.
func TestMigrateDryRunWritesNothing(t *testing.T) {
	original := releasedConfig(t, "v0.8.5")
	path := writeConfig(t, original)

	if err := migrateConfigCommand(path, false); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Error("a dry run edited the file")
	}
	if _, err := os.Stat(path + ".before-migrate"); !errors.Is(err, os.ErrNotExist) {
		t.Error("a dry run left a backup behind")
	}
}

// TestMigrateRefusesSilentlyAfterSayingWhy pairs with main's exit path: the
// command prints the reason itself because the failure reporter would flatten
// it, so the error it returns must be the one main knows not to print again.
func TestMigrateRefusesSilentlyAfterSayingWhy(t *testing.T) {
	path := writeConfig(t, releasedConfig(t, "v0.3.0"))
	err := migrateConfigCommand(path, true)
	if !errors.Is(err, errSilentRefusal) {
		t.Fatalf("err=%v, want the refusal main leaves unprinted", err)
	}
	if _, statErr := os.Stat(path + ".before-migrate"); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("a refusal still wrote")
	}
}

func TestMigrateWriteRefusesASymbolicLink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "managed-config.yaml")
	original := releasedConfig(t, "v0.8.5")
	if err := os.WriteFile(target, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "config.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	err := migrateConfigCommand(link, true)
	if err == nil || !strings.Contains(err.Error(), "symbolic-link") {
		t.Fatalf("err=%v, want symbolic-link refusal", err)
	}
	after, readErr := os.ReadFile(target)
	if readErr != nil || string(after) != original {
		t.Fatalf("symlink target changed: err=%v", readErr)
	}
	if _, statErr := os.Lstat(link + ".before-migrate"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("a refused symlink migration left a backup")
	}
}

func TestMigratePreservesRegularFileMode(t *testing.T) {
	path := writeConfig(t, releasedConfig(t, "v0.8.5"))
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := migrateConfigCommand(path, true); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + ".before-migrate"} {
		info, err := os.Stat(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o640 {
			t.Errorf("%s mode=%#o, want 0640", candidate, got)
		}
	}
}

func TestMigrateSyncFailureDoesNotReplaceTheOriginal(t *testing.T) {
	path := writeConfig(t, releasedConfig(t, "v0.8.5"))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	originalSync := syncConfigFile
	t.Cleanup(func() { syncConfigFile = originalSync })
	calls := 0
	syncConfigFile = func(file *os.File) error {
		calls++
		if calls == 2 {
			return fmt.Errorf("injected staging sync failure")
		}
		return file.Sync()
	}
	err = migrateConfigCommand(path, true)
	if err == nil || !strings.Contains(err.Error(), "injected staging sync failure") {
		t.Fatalf("err=%v, want injected sync failure", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || string(after) != string(original) {
		t.Fatalf("original changed after pre-rename sync failure: err=%v", readErr)
	}
	backup, readErr := os.ReadFile(path + ".before-migrate")
	if readErr != nil || string(backup) != string(original) {
		t.Fatalf("durable recovery copy missing after failure: err=%v", readErr)
	}
}

func TestMigrateRemovesABackupWhoseDurableWriteFailed(t *testing.T) {
	path := writeConfig(t, releasedConfig(t, "v0.8.5"))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	originalSync := syncConfigFile
	t.Cleanup(func() { syncConfigFile = originalSync })
	syncConfigFile = func(*os.File) error { return fmt.Errorf("injected backup sync failure") }
	err = migrateConfigCommand(path, true)
	if err == nil || !strings.Contains(err.Error(), "injected backup sync failure") {
		t.Fatalf("err=%v, want injected backup sync failure", err)
	}
	if _, statErr := os.Stat(path + ".before-migrate"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed backup permanently blocked retry: %v", statErr)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || string(after) != string(original) {
		t.Fatalf("original changed after backup failure: err=%v", readErr)
	}
}
