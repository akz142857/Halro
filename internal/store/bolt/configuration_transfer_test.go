package bolt

import (
	"context"
	"errors"
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
	validIntent := adminIntent("aud_import", "configuration.import", "configuration", "v1")
	if err := store.ImportPortableConfiguration(ctx, config, validIntent, invalidIntent); err == nil {
		t.Fatal("import succeeded without a valid audit event")
	}
	if err := store.CheckConfigurationImportTarget(); err != nil {
		t.Fatalf("rolled back import left records behind: %v", err)
	}
	if _, err := store.FindGatewayKeyByHash(ctx, key.KeyHash); err != ErrNotFound {
		t.Fatalf("rolled back import left a key index entry: %v", err)
	}
	if err := store.ImportPortableConfiguration(ctx, config, validIntent); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListPendingAdminAuditIntents(ctx)
	if err != nil || len(pending) != 1 || pending[0].EventID != "aud_import" {
		t.Fatalf("audit intent did not commit with the import: %v %v", pending, err)
	}
}

func TestConfigurationExportOmitsQuarantinedPrice(t *testing.T) {
	store, err := openForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	const deploymentID = "dep_review_quarantine"
	seedPricingDeployment(t, store, deploymentID, 0, 0, 0)
	backupAt := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	restoredAt := backupAt.Add(2 * time.Hour)
	scheduled := newStoredPrice("price_review_quarantine", deploymentID, backupAt.Add(time.Hour))
	if _, err := store.CreateDeploymentPriceVersion(ctx, scheduled); err != nil {
		t.Fatal(err)
	}
	if count, err := store.QuarantineRestoredScheduledPrices(ctx, backupAt, restoredAt); err != nil || count != 1 {
		t.Fatalf("quarantine count=%d err=%v", count, err)
	}
	if err := store.PricingReadiness(ctx); !errors.Is(err, ErrPricingQuarantined) {
		t.Fatalf("readiness: %v", err)
	}
	config, err := store.ReadPortableConfiguration(ctx, restoredAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := config.Prices[deploymentID]; ok || !config.QuarantinedPrices[deploymentID] {
		t.Fatal("quarantined scheduled price was not excluded and reported")
	}
}
