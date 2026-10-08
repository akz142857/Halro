package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type shortRestoreWriter struct{}

func (shortRestoreWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestRestoreReportJSONCompleteIDsAndActualWriterFailures(t *testing.T) {
	result := RestoreResult{BackupID: "complete", RestoredEnabledGatewayKeyCount: 2}
	ids := []string{"a\"escaped", "b"}
	walk := func(consume func(string) error) error {
		for _, id := range ids {
			if err := consume(id); err != nil {
				return err
			}
		}
		return nil
	}
	var output bytes.Buffer
	if err := WriteRestoreReportJSON(context.Background(), result, walk, &output); err != nil {
		t.Fatal(err)
	}
	var decoded RestoreResult
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil || !slices.Equal(decoded.RestoredEnabledGatewayKeyIDs, ids) {
		t.Fatal(output.String(), err)
	}
	if err := WriteRestoreReportJSON(context.Background(), result, walk, shortRestoreWriter{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	for _, mode := range []string{"incomplete", "extra", "source"} {
		output.Reset()
		sourceErr := errors.New("source stopped")
		bad := func(consume func(string) error) error {
			if err := consume("one"); err != nil {
				return err
			}
			if mode == "incomplete" {
				return nil
			}
			if mode == "source" {
				return sourceErr
			}
			if err := consume("two"); err != nil {
				return err
			}
			return consume("extra")
		}
		if err := WriteRestoreReportJSON(context.Background(), result, bad, &output); err == nil || json.Valid(output.Bytes()) {
			t.Fatal("failed report was completed", mode, err, output.String())
		}
	}
}

func streamedRestoreFixture(t *testing.T) (config.Config, string, []byte, string, []string) {
	t.Helper()
	cfg, bootstrap := journalledInstance(t)
	cfg = enableBackupReferenceBudget(cfg)
	store, err := openMetadataForTest(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	original, err := store.GetGatewayKey(context.Background(), bootstrap.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{bootstrap.KeyID}
	for i := 0; i < 130; i++ {
		item := original
		item.ID = fmt.Sprintf("key-restored-%03d", i)
		item.Name = item.ID
		item.KeyHash = sha256.Sum256([]byte(item.ID))
		item.Revision = 0
		if _, err := store.PutGatewayKey(context.Background(), item, 0, nil); err != nil {
			t.Fatal(err)
		}
		expected = append(expected, item.ID)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(expected)
	configPath := filepath.Join(filepath.Dir(cfg.Storage.DataDir), "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(filepath.Dir(cfg.Storage.DataDir), "restore-report.hmbk")
	key := bytes.Repeat([]byte{0x56}, 32)
	manifest, err := CreateBackupSummary(context.Background(), cfg, configPath, archive, key, CreateBackupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return cfg, archive, key, manifest.BackupID, expected
}

func TestDefaultRestoreReportStreamsAllEnabledIDsAndRetainsFeeThroughOutput(t *testing.T) {
	cfg, path, key, confirmation, expected := streamedRestoreFixture(t)
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	root, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	callbackCalled := false
	err = RestoreBackupWithReport(root, cfg, path, key, confirmation, RestoreOptions{}, func(result RestoreResult, walk func(func(string) error) error) error {
		callbackCalled = true
		if result.RestoredEnabledGatewayKeyIDs != nil || result.RestoredEnabledGatewayKeyCount != len(expected) || !result.VaultVerified {
			t.Fatal(result)
		}
		if snapshot := pressure.logicalBudget().Snapshot(); snapshot.Classes[resourcebudget.Recovery].RAM < restoreReportRAMBytes {
			t.Fatal("report escaped original job", snapshot)
		}
		return WriteRestoreReportJSON(root, result, walk, &output)
	})
	if err != nil || !callbackCalled {
		t.Fatal(callbackCalled, err)
	}
	var result RestoreResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || !slices.Equal(result.RestoredEnabledGatewayKeyIDs, expected) {
		t.Fatal("incomplete enabled IDs", len(result.RestoredEnabledGatewayKeyIDs), err)
	}
	if s := pressure.logicalBudget().Snapshot(); s.Classes[resourcebudget.Recovery].RAM >= restoreReportRAMBytes {
		t.Fatal("known output retained whole job", s)
	}
	entries, err := os.ReadDir(temp)
	if err != nil || len(entries) != 0 {
		t.Fatal("report workspace survived known close", entries, err)
	}
	// The visible result is the real restored native data, not just a streamed
	// fixture. The complete enabled-key set matches the actual candidate.
	store, err := openMetadataForTest(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var native []string
	if err := store.WalkGatewayKeysMaintenance(context.Background(), func(item domain.GatewayKey) error {
		if item.Enabled && item.DeletedAt == nil {
			native = append(native, item.ID)
		}
		return nil
	}); err != nil || !slices.Equal(native, expected) {
		t.Fatal(len(native), err)
	}
}

func TestRestoreReportForeignCatalogKeepsFilesAndOriginalFee(t *testing.T) {
	cfg, path, key, confirmation, _ := streamedRestoreFixture(t)
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	root, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var foreign string
	err = RestoreBackupWithReport(root, cfg, path, key, confirmation, RestoreOptions{}, func(_ RestoreResult, walk func(func(string) error) error) error {
		dirs, err := os.ReadDir(temp)
		if err != nil || len(dirs) != 1 {
			t.Fatal(dirs, err)
		}
		directory := filepath.Join(temp, dirs[0].Name())
		files, err := os.ReadDir(directory)
		if err != nil || len(files) != 1 {
			t.Fatal(files, err)
		}
		foreign = filepath.Join(directory, files[0].Name())
		if err := os.Rename(foreign, foreign+".saved"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(foreign, []byte("foreign"), 0600); err != nil {
			t.Fatal(err)
		}
		return walk(func(string) error { return nil })
	})
	if err == nil || foreign == "" {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(foreign); err != nil || string(raw) != "foreign" {
		t.Fatal(string(raw), err)
	}
	if _, err := os.Stat(foreign + ".saved"); err != nil {
		t.Fatal("escaped original inode removed", err)
	}
	if s := pressure.logicalBudget().Snapshot(); s.Classes[resourcebudget.Recovery].RAM < restoreReportRAMBytes {
		t.Fatal("uncertain retirement refunded job", s)
	}
}
