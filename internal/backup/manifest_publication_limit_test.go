package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/buildinfo"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func manifestLimitOptions(t *testing.T) CreateOptions {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, []byte("native fact"), 0600); err != nil {
		t.Fatal(err)
	}
	var files []SourceFile
	for _, name := range []string{"config/config.yaml", "data/metadata.db", "data/ledger/ledger.wal", "data/audit/audit.log"} {
		files = append(files, SourceFile{ArchivePath: name, LocalPath: source})
	}
	return CreateOptions{OutputPath: filepath.Join(root, "backup.hmbk"), BackupKey: bytes.Repeat([]byte{29}, 32), Files: files,
		Metadata:           boltstore.MetadataInfo{SchemaVersion: boltstore.CurrentSchemaVersion(), TxID: 1},
		LedgerFeatureEpoch: 4, MinimumLedgerReaderVersion: "v4", PricingStateSHA256: "sha256:" + strings.Repeat("1", 64), PendingIntentSHA256: "sha256:" + strings.Repeat("2", 64), MasterKeyFingerprint: "sha256:" + strings.Repeat("0", 64),
		Build: buildinfo.Info{Version: "test"}, Now: func() time.Time { return time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC) }}
}

func TestLegacyCreateRejectsUnsupportedManifestSizeBeforePublication(t *testing.T) {
	options := manifestLimitOptions(t)
	options.Build.Version = strings.Repeat("v", backupManifestSizeLimit)
	manifest, err := Create(options)
	if err == nil {
		_, verifyErr := Verify(options.OutputPath, options.BackupKey)
		t.Fatalf("Create published an archive outside the reader contract; Verify=%v", verifyErr)
	}
	if manifest.BackupID != "" || len(manifest.Files) != 0 {
		t.Fatal("failed creation returned a success manifest")
	}
	if _, err := os.Stat(options.OutputPath); !os.IsNotExist(err) {
		t.Fatalf("unsupported archive was published: %v", err)
	}
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(options.OutputPath), ".halro-backup-*.tmp"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("failed creation leaked temporary archive: %v %v", temps, err)
	}
}

func TestLegacyCreateManifestWithinReaderLimitRoundTrips(t *testing.T) {
	options := manifestLimitOptions(t)
	created, err := Create(options)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := Verify(options.OutputPath, options.BackupKey)
	if err != nil || verified.BackupID != created.BackupID || len(verified.Files) != 4 {
		t.Fatalf("created=%q verified=%q files=%d err=%v", created.BackupID, verified.BackupID, len(verified.Files), err)
	}
}
