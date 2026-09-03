package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

// Provider objects were written in the clear before they were sealed. An
// install carrying one of those cannot serve it — the reader now expects an
// envelope — so leaving it would be a resource that answers metadata and fails
// every read, with the caller's bytes still on disk. Startup removes both the
// file and the record it belonged to.
func TestStartupReclaimsProviderObjectsWrittenBeforeSealing(t *testing.T) {
	runtime, bootstrap, _ := activationTestRuntime(t)
	directory := filepath.Join(runtime.config.Storage.DataDir, "provider-objects")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}

	plaintextName := "file-legacy.object"
	if err := os.WriteFile(filepath.Join(directory, plaintextName), []byte(`{"prompt":"legacy"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	legacy := domain.ProviderResource{
		ID: "file-legacy", Kind: domain.ResourceFile, ProjectID: bootstrap.ProjectID,
		ProviderID: bootstrap.ProviderID, DeploymentID: bootstrap.DeploymentID,
		ProfileID: domain.ProfileOpenAIMediaResources, PublicModel: "chat",
		ObjectPath: plaintextName, ObjectContentType: "application/jsonl",
		CreationStatus: "completed", Status: "uploaded",
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if _, err := runtime.store.PutProviderResource(context.Background(), legacy, 0); err != nil {
		t.Fatal(err)
	}

	// A sealed object, and an orphan written in the clear that no record names.
	// The first must survive; the second is exactly what a write that stored its
	// bytes and then failed to store its record leaves behind.
	sealed, err := runtime.vault.EncryptResourceObject("file-sealed", bootstrap.ProjectID, []byte(`{"prompt":"current"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "file-sealed.object"), sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "file-orphan.object"), []byte("orphan plaintext"), 0o600); err != nil {
		t.Fatal(err)
	}

	runtime.reclaimUnsealedProviderObjects(context.Background())

	if _, err := os.Stat(filepath.Join(directory, plaintextName)); !os.IsNotExist(err) {
		t.Fatalf("the unsealed object survived startup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "file-orphan.object")); !os.IsNotExist(err) {
		t.Fatalf("an unsealed object no record named survived startup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "file-sealed.object")); err != nil {
		t.Fatalf("a sealed object was reclaimed too: %v", err)
	}
	if _, err := runtime.store.ProviderResource(context.Background(), bootstrap.ProjectID, "file-legacy"); err == nil {
		t.Fatal("the record for an unreadable object survived startup")
	}
}
