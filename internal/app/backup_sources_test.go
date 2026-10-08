package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/domain"
)

func TestBackupStreamsProviderInputAndSharedOutputReferences(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI, ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test", PublicModel: "chat", ProjectName: "Backup", BillingMode: domain.BillingModeFree}, []byte("provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := openMetadataForTest(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := domain.DefaultProviderProfile(domain.ProviderOpenAI)
	now := time.Now().UTC()
	for n := 0; n < 66; n++ {
		_, err := store.PutProviderResource(context.Background(), domain.ProviderResource{
			ID: fmt.Sprintf("file_shared_%03d", n), Kind: domain.ResourceFile, ProjectID: bootstrap.ProjectID, ProviderID: bootstrap.ProviderID, DeploymentID: bootstrap.DeploymentID,
			PublicModel: "chat", ProfileID: profile.ProfileID, ObjectPath: "answer.object", InputObjectPath: "request.object", CreationStatus: "completed", Status: "uploaded", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
		}, 0)
		if err != nil {
			store.Close()
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	objectDir := filepath.Join(cfg.Storage.DataDir, "provider-objects")
	if err := os.MkdirAll(objectDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"answer.object", "request.object"} {
		if err := os.WriteFile(filepath.Join(objectDir, name), []byte("sealed "+name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root := filepath.Dir(cfg.Storage.DataDir)
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "complete.hmbk")
	key := bytes.Repeat([]byte{91}, 32)
	manifest, err := CreateBackup(context.Background(), cfg, configPath, output, key)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := backup.Verify(output, key)
	if err != nil {
		t.Fatal(err)
	}
	if verified.BackupID != manifest.BackupID {
		t.Fatal("backup identity changed")
	}
	for _, name := range []string{"answer.object", "request.object"} {
		count := 0
		expected := sha256.Sum256([]byte("sealed " + name))
		for _, file := range verified.Files {
			if file.Path == "data/provider-objects/"+name {
				count++
				if file.SHA256 != hex.EncodeToString(expected[:]) {
					t.Fatal("shared object contents changed")
				}
			}
		}
		if count != 1 {
			t.Fatal("input/output shared reference was lost or duplicated", name, count)
		}
	}
	stages, err := filepath.Glob(filepath.Join(root, ".halro-backup-stage-*"))
	if err != nil || len(stages) != 0 {
		t.Fatal("backup stage/catalog leaked", stages, err)
	}
	// Missing input is refused even though the shared output still exists.
	if err := os.Remove(filepath.Join(objectDir, "request.object")); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "missing-input.hmbk")
	if _, err := CreateBackup(context.Background(), cfg, configPath, missing, key); err == nil {
		t.Fatal("missing input accepted")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("incomplete backup published", err)
	}
}
