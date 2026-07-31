package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/buildinfo"
	"github.com/akz142857/Heimdall/internal/ledger"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
)

func TestEncryptedBackupCreateVerifyAndSecretConfidentiality(t *testing.T) {
	root := t.TempDir()
	metadata := filepath.Join(root, "metadata.db")
	wal := filepath.Join(root, "ledger.wal")
	config := filepath.Join(root, "config.yaml")
	audit := filepath.Join(root, "audit.log")
	const canary = "provider-secret-backup-canary"
	if err := os.WriteFile(metadata, []byte("metadata:"+canary), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wal, []byte("ledger"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("version: 1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audit, []byte("audit"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x42}, 32)
	output := filepath.Join(root, "backup.hmbk")
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	created, err := Create(CreateOptions{
		OutputPath: output, BackupKey: key,
		Files: []SourceFile{
			{ArchivePath: "config/config.yaml", LocalPath: config},
			{ArchivePath: "data/audit/audit.log", LocalPath: audit},
			{ArchivePath: "data/ledger/ledger.wal", LocalPath: wal},
			{ArchivePath: "data/metadata.db", LocalPath: metadata},
		},
		Metadata:             boltstore.MetadataInfo{SchemaVersion: 2, TxID: 7},
		LedgerWatermark:      ledger.Watermark{Generation: 1, Offset: 123, Sequence: 4},
		CheckpointWatermark:  ledger.Watermark{Generation: 1, Offset: 100, Sequence: 3},
		UsageManifestVersion: 1,
		MasterKeyFingerprint: "sha256:" + strings.Repeat("0", 64),
		Build:                buildinfo.Info{Version: "test"}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte(canary)) || bytes.Contains(payload, []byte("manifest.json")) {
		t.Fatal("encrypted backup exposed plaintext")
	}
	verified, err := Verify(output, key)
	if err != nil {
		t.Fatal(err)
	}
	if verified.BackupID != created.BackupID || verified.Metadata != created.Metadata ||
		len(verified.Files) != 4 {
		t.Fatalf("verified manifest=%#v", verified)
	}
	wrongKey := bytes.Repeat([]byte{0x24}, 32)
	if _, err := Verify(output, wrongKey); err == nil {
		t.Fatal("wrong backup key was accepted")
	}
}

func TestEncryptedBackupRejectsTamperAndTruncation(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, bytes.Repeat([]byte("x"), chunkSize+100), 0o600); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{1}, 32)
	original := filepath.Join(root, "backup.hmbk")
	if _, err := Create(CreateOptions{
		OutputPath: original, BackupKey: key,
		Files: []SourceFile{{ArchivePath: "data/source", LocalPath: source}},
		Now:   func() time.Time { return time.Now().UTC() },
	}); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutated := range map[string][]byte{
		"tampered": func() []byte {
			copyPayload := bytes.Clone(payload)
			copyPayload[len(copyPayload)/2] ^= 0xff
			return copyPayload
		}(),
		"truncated": bytes.Clone(payload[:len(payload)-1]),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(root, name+".hmbk")
			if err := os.WriteFile(path, mutated, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Verify(path, key); err == nil {
				t.Fatal("corrupt backup was accepted")
			}
		})
	}
}

func TestBackupPublicationDoesNotOverwriteAndRejectsUnsafeSources(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{2}, 32)
	output := filepath.Join(root, "backup.hmbk")
	if err := os.WriteFile(output, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Create(CreateOptions{
		OutputPath: output, BackupKey: key,
		Files: []SourceFile{{ArchivePath: "data/source", LocalPath: source}},
	})
	if err == nil {
		t.Fatal("existing output was overwritten")
	}
	payload, err := os.ReadFile(output)
	if err != nil || string(payload) != "existing" {
		t.Fatal("existing output was modified")
	}
	_, err = Create(CreateOptions{
		OutputPath: filepath.Join(root, "unsafe.hmbk"), BackupKey: key,
		Files: []SourceFile{{ArchivePath: "../escape", LocalPath: source}},
	})
	if err == nil {
		t.Fatal("unsafe archive path was accepted")
	}
	symlink := filepath.Join(root, "link")
	if err := os.Symlink(source, symlink); err != nil {
		t.Fatal(err)
	}
	_, err = Create(CreateOptions{
		OutputPath: filepath.Join(root, "new.hmbk"), BackupKey: key,
		Files: []SourceFile{{ArchivePath: "data/source", LocalPath: symlink}},
		Now:   func() time.Time { return time.Now().UTC() },
	})
	if err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink source error=%v", err)
	}
	hardlink := filepath.Join(root, "hardlink")
	if err := os.Link(source, hardlink); err != nil {
		t.Fatal(err)
	}
	_, err = Create(CreateOptions{
		OutputPath: filepath.Join(root, "hardlink.hmbk"), BackupKey: key,
		Files: []SourceFile{{ArchivePath: "data/source", LocalPath: source}},
		Now:   func() time.Time { return time.Now().UTC() },
	})
	if err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Fatalf("hardlink source error=%v", err)
	}
}

func TestLoadBackupKeyFileRequiresExactLengthAndPrivateMode(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "backup.key")
	if err := os.WriteFile(path, bytes.Repeat([]byte{7}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := LoadKeyFile(path)
	if err != nil || len(key) != 32 {
		t.Fatalf("key length=%d err=%v", len(key), err)
	}
	clear(key)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyFile(path); err == nil {
		t.Fatal("group/world-readable backup key was accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte{7}, 31), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyFile(path); err == nil {
		t.Fatal("short backup key was accepted")
	}
}
