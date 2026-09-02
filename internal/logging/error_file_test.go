package logging

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/config"
)

func loggingConfig(t *testing.T, mutate func(*config.Config)) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.DataDir = t.TempDir()
	if mutate != nil {
		mutate(&cfg)
	}
	return cfg
}

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		// Every line stands on its own: this file is grepped and shipped, and
		// one that has to be parsed as a whole document cannot be tailed.
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line is not independently decodable: %s", line)
		}
		records = append(records, record)
	}
	return records
}

// The whole point of a second file: the main log keeps everything it was
// configured for, and the errors-only copy holds the errors alone. Setting
// `level: error` would have got the second half by throwing away the first.
func TestTheErrorFileHoldsErrorsAndTheMainLogHoldsEverything(t *testing.T) {
	cfg := loggingConfig(t, func(cfg *config.Config) {
		cfg.Logging.Output = config.LogOutputFile
		cfg.Logging.Level = "info"
		cfg.Logging.ErrorFile.Enabled = true
	})
	logger, controls, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer controls.Close()

	logger.Info("logging configured")
	logger.Warn("provider attempt failed", "error_class", "timeout")
	logger.Error("request failed", "request_id", "req_1", "error_class", "authentication")

	errors := readLines(t, cfg.ErrorLogFilePath())
	if len(errors) != 1 || errors[0]["msg"] != "request failed" || errors[0]["level"] != "ERROR" {
		t.Fatalf("error file = %v", errors)
	}
	if errors[0]["request_id"] != "req_1" {
		t.Fatalf("the error file dropped the record's attributes: %v", errors[0])
	}
	main := readLines(t, cfg.LogFilePath())
	if len(main) != 3 {
		t.Fatalf("the main log lost records to the error file: %v", main)
	}
}

// A WARN worth keeping is exactly what `level: error` costs, so the file that
// replaces that setting must not take one.
func TestTheErrorFileTakesNoWarnings(t *testing.T) {
	cfg := loggingConfig(t, func(cfg *config.Config) {
		cfg.Logging.Output = config.LogOutputFile
		cfg.Logging.ErrorFile.Enabled = true
	})
	logger, controls, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer controls.Close()

	logger.Debug("routing decision")
	logger.Info("certificate loaded")
	logger.Warn("certificate expires in 6 days")

	raw, err := os.ReadFile(cfg.ErrorLogFilePath())
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(string(raw))) != 0 {
		t.Fatalf("the error file took a record below ERROR: %s", raw)
	}
}

// The threshold is fixed, not the live one. Turning the main log up to debug
// during an incident must not turn the errors-only file into a second copy of
// it — which is what a shared LevelVar would have done.
func TestTheErrorFileIgnoresALiveLevelChange(t *testing.T) {
	cfg := loggingConfig(t, func(cfg *config.Config) {
		cfg.Logging.Output = config.LogOutputFile
		cfg.Logging.ErrorFile.Enabled = true
	})
	logger, controls, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer controls.Close()

	controls.SetLevel(slog.LevelDebug)
	logger.Debug("routing decision")
	logger.Info("pricing snapshot selected")
	controls.SetLevel(slog.LevelError)
	logger.Error("request failed")

	records := readLines(t, cfg.ErrorLogFilePath())
	if len(records) != 1 || records[0]["msg"] != "request failed" {
		t.Fatalf("error file = %v", records)
	}
}

// Redaction is a property of the log, not of a destination. The fan-out sits
// inside safelog for this: two already-wrapped loggers side by side would make
// "is this redacted" depend on which one a call site reached for.
func TestBothDestinationsAreRedacted(t *testing.T) {
	cfg := loggingConfig(t, func(cfg *config.Config) {
		cfg.Logging.Output = config.LogOutputFile
		cfg.Logging.ErrorFile.Enabled = true
	})
	logger, controls, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer controls.Close()

	const canary = "gw_livecanarykeymaterial0123456789"
	logger.Error("request failed", "authorization", "Bearer "+canary)

	for _, path := range []string{cfg.LogFilePath(), cfg.ErrorLogFilePath()} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), canary) {
			t.Fatalf("%s holds a credential: %s", path, raw)
		}
	}
}

// One SIGHUP has to move both files, or logrotate rotates the pair and the
// process keeps writing one of them into an unlinked inode.
func TestReopenAndCloseCoverEveryFile(t *testing.T) {
	cfg := loggingConfig(t, func(cfg *config.Config) {
		cfg.Logging.Output = config.LogOutputFile
		cfg.Logging.ErrorFile.Enabled = true
	})
	logger, controls, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	logger.Error("request failed", "request_id", "req_before")
	if !controls.HasFile() {
		t.Fatal("HasFile denied the files it holds open")
	}
	// What logrotate does: move both aside, then signal.
	for _, path := range []string{cfg.LogFilePath(), cfg.ErrorLogFilePath()} {
		if err := os.Rename(path, path+".rotated"); err != nil {
			t.Fatal(err)
		}
	}
	if err := controls.ReopenFile(); err != nil {
		t.Fatal(err)
	}
	logger.Error("request failed", "request_id", "req_after")
	if err := controls.Close(); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{cfg.LogFilePath(), cfg.ErrorLogFilePath()} {
		records := readLines(t, path)
		if len(records) != 1 || records[0]["request_id"] != "req_after" {
			t.Fatalf("%s did not reopen: %v", path, records)
		}
	}
}

// Off is off: no second file appears, and the ordinary log is unchanged.
func TestTheErrorFileIsNotCreatedWhenDisabled(t *testing.T) {
	cfg := loggingConfig(t, func(cfg *config.Config) {
		cfg.Logging.Output = config.LogOutputFile
	})
	logger, controls, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer controls.Close()
	logger.Error("request failed")

	if _, err := os.Stat(cfg.ErrorLogFilePath()); !os.IsNotExist(err) {
		t.Fatalf("an error file was created while disabled: %v", err)
	}
	if records := readLines(t, cfg.LogFilePath()); len(records) != 1 {
		t.Fatalf("the main log changed: %v", records)
	}
}

// Rotation is what bounds the disk, and this file's generations are what an
// operator reads back through after an incident.
func TestTheErrorFileRotatesWithinItsGenerationLimit(t *testing.T) {
	cfg := loggingConfig(t, func(cfg *config.Config) {
		cfg.Logging.Output = config.LogOutputStderr
		cfg.Logging.ErrorFile.Enabled = true
		cfg.Logging.ErrorFile.MaxSizeMB = 1
		cfg.Logging.ErrorFile.MaxFiles = 2
	})
	logger, controls, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer controls.Close()

	filler := strings.Repeat("x", 4096)
	for range 900 {
		logger.Error("request failed", "detail", filler)
	}

	directory := filepath.Dir(cfg.ErrorLogFilePath())
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	generations := 0
	base := filepath.Base(cfg.ErrorLogFilePath())
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), base) {
			generations++
		}
	}
	if generations == 0 || generations > cfg.Logging.ErrorFile.MaxFiles {
		t.Fatalf("%d generations kept, limit is %d", generations, cfg.Logging.ErrorFile.MaxFiles)
	}
}

// The permissions of the data directory it sits in. Records are redacted before
// they arrive, but that is a property of the writer and not a licence for the
// file to be world-readable.
func TestTheErrorFileIsAsPrivateAsTheDataDirectory(t *testing.T) {
	cfg := loggingConfig(t, func(cfg *config.Config) {
		cfg.Logging.Output = config.LogOutputStderr
		cfg.Logging.ErrorFile.Enabled = true
	})
	logger, controls, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer controls.Close()
	logger.Error("request failed")

	info, err := os.Stat(cfg.ErrorLogFilePath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != FilePerm {
		t.Fatalf("mode = %v, want %v", info.Mode().Perm(), FilePerm)
	}
	directory, err := os.Stat(filepath.Dir(cfg.ErrorLogFilePath()))
	if err != nil {
		t.Fatal(err)
	}
	if directory.Mode().Perm() != DirPerm {
		t.Fatalf("directory mode = %v, want %v", directory.Mode().Perm(), DirPerm)
	}
}
