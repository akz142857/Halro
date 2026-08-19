package app

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"gopkg.in/yaml.v3"
)

// stubLogControls stands in for the live process log. It records what a reload
// asked of it, which is what the tests below assert on.
type stubLogControls struct {
	level       slog.Level
	hasFile     bool
	reopens     int
	reopenError error
}

func (s *stubLogControls) SetLevel(level slog.Level) { s.level = level }
func (s *stubLogControls) Level() slog.Level         { return s.level }
func (s *stubLogControls) HasFile() bool             { return s.hasFile }
func (s *stubLogControls) ReopenFile() error {
	s.reopens++
	return s.reopenError
}

func reloadCounts(t *testing.T, runtime *Runtime, item string) (uint64, uint64, bool) {
	t.Helper()
	state := runtime.reload.state.snapshot()[item]
	return state.success, state.failure, state.lastSuccessUnix > 0
}

func TestReloadAppliesEachItemIndependently(t *testing.T) {
	directory := t.TempDir()
	expiry := time.Now().Add(time.Hour)
	entry, firstPrint := writeKeypair(t, directory, "serving", expiry, "halro.example.com")
	holder, err := newCertificateHolder([]config.TLSCertificate{entry})
	if err != nil {
		t.Fatal(err)
	}
	controls := &stubLogControls{level: slog.LevelInfo, hasFile: true}
	runtime := &Runtime{
		logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		reload: reloadRuntime{serving: holder, logControls: controls},
	}

	if err := runtime.Reload(); err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if success, failure, recorded := reloadCounts(t, runtime, ReloadTLS); success != 1 || failure != 0 || !recorded {
		t.Fatalf("tls reload state success=%d failure=%d recorded=%v", success, failure, recorded)
	}
	if controls.reopens != 1 {
		t.Fatalf("log file reopens=%d want 1", controls.reopens)
	}
	// Metrics TLS is not configured here, so it must record neither outcome:
	// "not applicable" and "never succeeded" are different answers.
	if success, failure, recorded := reloadCounts(t, runtime, ReloadMetricsTLS); success != 0 || failure != 0 || recorded {
		t.Fatalf("an unconfigured item recorded an outcome: success=%d failure=%d recorded=%v", success, failure, recorded)
	}
	// The log level needs a configuration file to read a new value from; without
	// one it is skipped rather than counted.
	if success, failure, _ := reloadCounts(t, runtime, ReloadLogLevel); success != 0 || failure != 0 {
		t.Fatalf("log level was applied without a configuration path: success=%d failure=%d", success, failure)
	}

	_, secondPrint := writeKeypair(t, directory, "serving", expiry, "halro.example.com")
	if err := runtime.Reload(); err != nil {
		t.Fatalf("second reload failed: %v", err)
	}
	if got := servedFingerprint(t, holder, "halro.example.com"); got != secondPrint || got == firstPrint {
		t.Fatal("the second reload did not pick up the replaced certificate")
	}
}

func TestReloadKeepsServingWhenOneItemFails(t *testing.T) {
	directory := t.TempDir()
	entry, firstPrint := writeKeypair(t, directory, "serving", time.Now().Add(time.Hour), "halro.example.com")
	holder, err := newCertificateHolder([]config.TLSCertificate{entry})
	if err != nil {
		t.Fatal(err)
	}
	controls := &stubLogControls{level: slog.LevelInfo, hasFile: true}
	runtime := &Runtime{
		logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1})),
		reload: reloadRuntime{serving: holder, logControls: controls},
	}
	if err := os.WriteFile(entry.KeyFile, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = runtime.Reload()
	if err == nil {
		t.Fatal("a broken keypair produced no error")
	}
	if !strings.Contains(err.Error(), ReloadTLS) {
		t.Fatalf("the failure did not name the item: %v", err)
	}
	// The failure must not stop the other items, and must not disturb what is
	// already being served.
	if controls.reopens != 1 {
		t.Fatalf("a TLS failure prevented the log file reopen: reopens=%d", controls.reopens)
	}
	if got := servedFingerprint(t, holder, "halro.example.com"); got != firstPrint {
		t.Fatal("a failed reload changed the certificate being served")
	}
	if success, failure, recorded := reloadCounts(t, runtime, ReloadTLS); success != 0 || failure != 1 || recorded {
		t.Fatalf("tls failure state success=%d failure=%d recorded=%v", success, failure, recorded)
	}
}

func TestReloadPicksUpTheLogLevelFromTheConfigurationFile(t *testing.T) {
	cfg := testConfig(t)
	cfg.Logging = config.Logging{Level: "info", Format: config.LogFormatJSON, Output: config.LogOutputStderr, MaxSizeMB: 64, MaxFiles: 5}
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfigFile(t, path, cfg)

	controls := &stubLogControls{level: slog.LevelInfo}
	runtime := &Runtime{
		logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1})),
		config: cfg,
		reload: reloadRuntime{configPath: path, logControls: controls},
	}
	if err := runtime.Reload(); err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if controls.level != slog.LevelInfo {
		t.Fatalf("an unchanged file moved the level to %s", controls.level)
	}

	cfg.Logging.Level = "debug"
	writeConfigFile(t, path, cfg)
	if err := runtime.Reload(); err != nil {
		t.Fatalf("reload after edit failed: %v", err)
	}
	if controls.level != slog.LevelDebug {
		t.Fatalf("the new level was not applied: %s", controls.level)
	}
	if success, failure, recorded := reloadCounts(t, runtime, ReloadLogLevel); success != 2 || failure != 0 || !recorded {
		t.Fatalf("log level state success=%d failure=%d recorded=%v", success, failure, recorded)
	}
	// The effective configuration must report the level in force rather than the
	// one the process started with.
	if effective := runtime.effectiveConfig(); effective.Logging.Level != "debug" {
		t.Fatalf("effective configuration still reports %q", effective.Logging.Level)
	}
}

func TestReloadRefusesALogLevelFromAConfigurationThatWouldNotStart(t *testing.T) {
	cfg := testConfig(t)
	cfg.Logging = config.Logging{Level: "info", Format: config.LogFormatJSON, Output: config.LogOutputStderr, MaxSizeMB: 64, MaxFiles: 5}
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfigFile(t, path, cfg)

	broken := cfg
	broken.Logging.Level = "debug"
	// A value the loader refuses. Taking the level out of this file would mean
	// running with a setting from a configuration the process could not start on.
	broken.Usage.Durability = "whenever"
	writeConfigFile(t, path, broken)

	controls := &stubLogControls{level: slog.LevelInfo}
	runtime := &Runtime{
		logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1})),
		config: cfg,
		reload: reloadRuntime{configPath: path, logControls: controls},
	}
	if err := runtime.Reload(); err == nil {
		t.Fatal("an invalid configuration file was accepted")
	}
	if controls.level != slog.LevelInfo {
		t.Fatalf("a refused configuration still moved the level to %s", controls.level)
	}
	if success, failure, _ := reloadCounts(t, runtime, ReloadLogLevel); success != 0 || failure != 1 {
		t.Fatalf("log level state success=%d failure=%d", success, failure)
	}
}

func TestReloadStatusSeparatesInapplicableFromNeverRun(t *testing.T) {
	runtime := &Runtime{logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))}
	status := runtime.reloadStatus()
	items, ok := status["items"].([]reloadItemStatus)
	if !ok || len(items) != len(reloadItems) {
		t.Fatalf("unexpected reload status shape: %#v", status["items"])
	}
	for _, item := range items {
		if item.Applies {
			t.Fatalf("item %q claims to apply on a runtime with nothing configured", item.Item)
		}
		if item.LastSuccess != nil {
			t.Fatalf("item %q reports a last success before any reload", item.Item)
		}
	}
}

// writeConfigFile renders a Config the way config.Load will read it back. It
// writes YAML rather than hand-built text so a field added to Config does not
// silently drop out of these fixtures.
func writeConfigFile(t *testing.T, path string, cfg config.Config) {
	t.Helper()
	payload, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
}
