package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// §15 requires backup and restore to preserve the capability snapshot. It rides
// along structurally, because the archive carries the whole metadata database —
// but "structurally" is a claim about today's implementation, not a contract,
// and nothing asserted it. A backup that silently lost the snapshot would leave
// every restored deployment unable to validate and unable to route.
func TestBackupRestorePreservesTheCapabilitySnapshot(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test",
		PublicModel: "chat", ProjectName: "Snapshot", BillingMode: domain.BillingModeFree,
	}, []byte("provider-secret"))
	if err != nil {
		t.Fatal(err)
	}

	before := readDeploymentSnapshot(t, cfg, bootstrap.DeploymentID)
	if before.Source == "" || before.ModelRevision == "" || before.CapturedAt.IsZero() {
		t.Fatalf("bootstrap produced no usable snapshot to test with: %+v", before)
	}

	root := filepath.Dir(cfg.Storage.DataDir)
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x3c}, 32)
	archive := filepath.Join(root, "snapshot.hmbk")
	manifest, err := CreateBackup(context.Background(), cfg, configPath, archive, key)
	if err != nil {
		t.Fatal(err)
	}

	// Move the deployment on after the backup, so a restore that quietly kept
	// live data would be visible rather than looking like success.
	mutateDeploymentSnapshot(t, cfg, bootstrap.DeploymentID)
	if after := readDeploymentSnapshot(t, cfg, bootstrap.DeploymentID); after.ModelRevision == before.ModelRevision {
		t.Fatal("the mutation did not take, so the restore below would prove nothing")
	}

	if _, err := RestoreBackup(context.Background(), cfg, archive, key, manifest.BackupID); err != nil {
		t.Fatal(err)
	}

	restored := readDeploymentSnapshot(t, cfg, bootstrap.DeploymentID)
	if !reflect.DeepEqual(restored, before) {
		t.Fatalf("restored snapshot differs:\n before=%+v\n after =%+v", before, restored)
	}

	// A restored deployment must still be usable: it validates, and the runtime
	// opens on the restored directory.
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("runtime refused the restored directory: %v", err)
	}
	defer runtime.Close()
	deployment, err := runtime.store.GetDeployment(context.Background(), bootstrap.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := deployment.Validate(); err != nil {
		t.Fatalf("restored deployment does not validate: %v", err)
	}
}

func readDeploymentSnapshot(t *testing.T, cfg config.Config, id string) domain.ModelCapabilitySnapshot {
	t.Helper()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	deployment, err := store.GetDeployment(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return deployment.ModelCapabilitySnapshot
}

func mutateDeploymentSnapshot(t *testing.T, cfg config.Config, id string) {
	t.Helper()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	deployment, err := store.GetDeployment(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	deployment.ModelCapabilitySnapshot.ModelRevision = "sha256:changed-after-backup"
	if _, err := store.PutDeployment(context.Background(), deployment, deployment.Revision, nil); err != nil {
		t.Fatal(err)
	}
}
