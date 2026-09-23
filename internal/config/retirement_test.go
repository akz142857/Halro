package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// releasesDir holds one snapshot of default.yaml per published release. It is
// the only fixture here that is not written by hand, and that is the point: a
// migrator tested against invented old configs tests the author's idea of an
// old config. Adding a release adds its file.
const releasesDir = "testdata/releases"

func releaseSnapshots(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(releasesDir)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".yaml") {
			paths = append(paths, filepath.Join(releasesDir, entry.Name()))
		}
	}
	if len(paths) == 0 {
		t.Fatalf("no release snapshots in %s", releasesDir)
	}
	return paths
}

// TestEveryReleasedConfigIsAnswered is the check that was missing when
// circuit_breaker retired. Measured against the real binary at the time: all
// twelve published default.yaml files were refused by the current tree, eleven
// of them on circuit_breaker and v0.3.0 also on elevation_window — and nothing
// in the tree noticed, because nothing had ever loaded a released config.
//
// A released config is allowed to be refused. It is not allowed to be refused
// by a sentence naming a Go type.
func TestEveryReleasedConfigIsAnswered(t *testing.T) {
	for _, path := range releaseSnapshots(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, loadErr := Decode(strings.NewReader(string(source)))
			if loadErr == nil {
				return
			}
			var retired *RetiredKeyError
			if !errors.As(loadErr, &retired) {
				t.Fatalf("refused by something other than the retirement table, so the "+
					"operator gets a decoder error instead of the key that replaced theirs: %v", loadErr)
			}
		})
	}
}

// TestEveryReleasedConfigLoadsAfterMigration is the sentence the whole table
// exists to make true: take any released configuration, run the migration the
// binary offers, and the result must load. Where the migration refuses, the
// refusal must name the key and the reason rather than leaving the operator to
// read a diff.
func TestEveryReleasedConfigLoadsAfterMigration(t *testing.T) {
	for _, path := range releaseSnapshots(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			result, err := Migrate(source)
			if err != nil {
				t.Fatalf("migrate: %v", err)
			}
			if len(result.Refusals) > 0 {
				for _, refusal := range result.Refusals {
					if !strings.Contains(refusal, " is now ") {
						t.Errorf("refusal does not name the replacement: %q", refusal)
					}
				}
				return
			}
			cfg, err := Decode(strings.NewReader(string(result.Output)))
			if err != nil {
				t.Fatalf("migrated config does not decode: %v", err)
			}
			if err := cfg.Normalize(); err != nil {
				t.Fatalf("migrated config does not normalize: %v", err)
			}
			if err := cfg.Validate(LoadOptions{}); err != nil {
				t.Fatalf("migrated config does not validate: %v", err)
			}
		})
	}
}

// TestMigrationCarriesTheOperatorsValue is what separates the migration from
// "delete the section and start again". Deleting is enough to make the file
// load — every routing key takes its default when absent — so the only thing
// that can be lost is the value the operator chose, which is exactly the thing
// worth keeping.
func TestMigrationCarriesTheOperatorsValue(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(releasesDir, "v0.8.5.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	tuned := strings.NewReplacer(
		"consecutive_failures: 5", "consecutive_failures: 9",
		"open_duration: 30s", "open_duration: 45s",
		"half_open_max_requests: 1", "half_open_max_requests: 3",
	).Replace(string(source))
	if tuned == string(source) {
		t.Fatal("the fixture no longer carries the circuit_breaker values this test tunes")
	}

	result, err := Migrate([]byte(tuned))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Refusals) > 0 {
		t.Fatalf("refused a mechanical migration: %v", result.Refusals)
	}
	if err := refuseRetiredKeys(result.Output); err != nil {
		t.Errorf("a retired key survived the migration: %v", err)
	}

	cfg, err := Load(writeTemp(t, result.Output), LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routing.AvailabilityFailures != 9 {
		t.Errorf("availability_failures=%d, want the 9 the operator set as consecutive_failures", cfg.Routing.AvailabilityFailures)
	}
	if time.Duration(cfg.Routing.SuspendFor) != 45*time.Second {
		t.Errorf("suspend_for=%s, want the 45s the operator set as open_duration", time.Duration(cfg.Routing.SuspendFor))
	}
	if cfg.Routing.ProbeRequests != 3 {
		t.Errorf("probe_requests=%d, want the 3 the operator set as half_open_max_requests", cfg.Routing.ProbeRequests)
	}
	// Absent means the default, so the one key with no predecessor is not
	// written. Writing it would be a value this migrator invented.
	if strings.Contains(string(result.Output), "max_suspend_for") {
		t.Error("max_suspend_for was written; it has no predecessor and absent already means the default")
	}
}

// TestMigrationRefusesWhatNeedsJudgement holds the line between a rename and a
// decision. elevation_window's replacement covers every step-up endpoint rather
// than capability detection alone, so carrying the operator's value across
// would widen a security window on their behalf.
func TestMigrationRefusesWhatNeedsJudgement(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(releasesDir, "v0.3.0.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := Migrate(source)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Refusals) == 0 {
		t.Fatal("migrated a key whose meaning changed")
	}
	if len(result.Output) != 0 {
		t.Error("a refusal wrote output; a half-migrated file is worse than an unmigrated one")
	}
	if !strings.Contains(result.Refusals[0], "elevation_window") {
		t.Errorf("refusal does not name the key: %q", result.Refusals[0])
	}
}

// TestRetirementRefusalNamesTheReplacement keeps the table from growing a row
// that says nothing. The whole reason it exists is that a decoder error names a
// Go type, so a row without a replacement or a reason is no better.
func TestRetirementRefusalNamesTheReplacement(t *testing.T) {
	for _, retirement := range Retirements() {
		if retirement.Path == "" || retirement.Why == "" {
			t.Errorf("retirement %+v has no path or no reason", retirement)
		}
		if retirement.ReplacedBy == retirement.Path {
			t.Errorf("%s is recorded as replacing itself", retirement.Path)
		}
	}
}

// TestMigrateLeavesACurrentConfigAlone is the no-op case: the file the binary
// writes today holds no retired key, so running the migration must not touch it.
func TestMigrateLeavesACurrentConfigAlone(t *testing.T) {
	result, err := Migrate(defaultTemplate)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Actions) != 0 || len(result.Refusals) != 0 {
		t.Fatalf("actions=%v refusals=%v, want a current config left alone", result.Actions, result.Refusals)
	}
	if string(result.Output) != string(defaultTemplate) {
		t.Error("the migration rewrote a file that had nothing to migrate")
	}
}

func writeTemp(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestMigrationRefusesOutputTheLoaderWouldReject is the guarantee that makes
// the command safe to run before an upgrade: a file it wrote passes
// `halro config check`. open_duration had no ceiling to agree with; its
// replacement must not exceed routing.max_suspend_for, so a long-enough value
// migrates into a file that would not start.
func TestMigrationRefusesOutputTheLoaderWouldReject(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(releasesDir, "v0.8.5.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	tuned := strings.Replace(string(source), "open_duration: 30s", "open_duration: 10m", 1)
	if tuned == string(source) {
		t.Fatal("the fixture no longer carries open_duration")
	}
	result, err := Migrate([]byte(tuned))
	if err == nil {
		t.Fatalf("migrated a config the loader rejects, output=%d bytes", len(result.Output))
	}
	if !strings.Contains(err.Error(), "max_suspend_for") {
		t.Errorf("error does not name the problem: %v", err)
	}
	if len(result.Output) != 0 {
		t.Error("output was handed back alongside the error")
	}
}

// TestMigrationRefusesWhenBothKeysAreSet keeps the migration from silently
// picking a winner. An operator part-way through the edit by hand has both, and
// writing the retired value across would either duplicate the key or overwrite
// the new one.
func TestMigrationRefusesWhenBothKeysAreSet(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(releasesDir, "v0.8.5.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	both := string(source) + "\nrouting:\n  availability_failures: 7\n"
	result, err := Migrate([]byte(both))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Refusals) == 0 {
		t.Fatal("migrated onto a key the operator had already set")
	}
	if !strings.Contains(result.Refusals[0], "routing.availability_failures") {
		t.Errorf("refusal does not name the destination: %q", result.Refusals[0])
	}
	if len(result.Output) != 0 {
		t.Error("a refusal wrote output")
	}
}

// TestMigrationWritesIntoAnExistingSection covers the other half of that: a
// destination section that exists but does not yet hold the key being moved.
func TestMigrationWritesIntoAnExistingSection(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(releasesDir, "v0.8.5.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	partial := strings.Replace(string(source), "open_duration: 30s", "open_duration: 10m", 1) +
		"\nrouting:\n  max_suspend_for: 15m\n"
	result, err := Migrate([]byte(partial))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Refusals) > 0 {
		t.Fatalf("refused: %v", result.Refusals)
	}
	cfg, err := Load(writeTemp(t, result.Output), LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(cfg.Routing.SuspendFor) != 10*time.Minute {
		t.Errorf("suspend_for=%s, want 10m", time.Duration(cfg.Routing.SuspendFor))
	}
	if time.Duration(cfg.Routing.MaxSuspendFor) != 15*time.Minute {
		t.Errorf("max_suspend_for=%s, want the 15m the operator had already written", time.Duration(cfg.Routing.MaxSuspendFor))
	}
}
