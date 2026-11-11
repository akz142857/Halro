package bolt

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/domain"
)

func TestConfigurationImportRollsBackRecordsAndKeyIndexWhenAuditFails(t *testing.T) {
	ctx := context.Background()
	store, err := openForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	project := domain.Project{ID: "prj_import", Name: "Test project", CreatedAt: now, UpdatedAt: now}
	_, key, err := auth.GenerateGatewayKeyWithID("key_import", project.ID, "Test key", nil)
	if err != nil {
		t.Fatal(err)
	}
	config := PortableConfiguration{Projects: []domain.Project{project}, GatewayKeys: []domain.GatewayKey{key}}
	invalidIntent := adminIntent("", "configuration.import", "configuration", "v1")
	if err := store.ImportPortableConfiguration(ctx, config, invalidIntent); err == nil {
		t.Fatal("import succeeded without a valid audit event")
	}
	if err := store.CheckConfigurationImportTarget(); err != nil {
		t.Fatalf("rolled back import left records behind: %v", err)
	}
	if _, err := store.FindGatewayKeyByHash(ctx, key.KeyHash); err != ErrNotFound {
		t.Fatalf("rolled back import left a key index entry: %v", err)
	}
	if err := store.ImportPortableConfiguration(ctx, config, adminIntent("aud_import", "configuration.import", "configuration", "v1")); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListPendingAdminAuditIntents(ctx)
	if err != nil || len(pending) != 1 || pending[0].EventID != "aud_import" {
		t.Fatalf("audit intent did not commit with the import: %v %v", pending, err)
	}
}
