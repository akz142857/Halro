package bolt

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOfflinePricingMigrationBindsResolutionToMetadataDigestAndPublishesStagedCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := DryRunPricingMigration(context.Background(), path)
	if err != nil || report.SchemaVersion != 1 || report.MetadataSHA256 == "" {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	if _, err := ApplyPricingMigration(context.Background(), path, PricingMigrationResolutionFile{SchemaVersion: 1, MetadataSHA256: "sha256:wrong", Operator: "test", Resolutions: map[string]PricingMigrationResolution{}}); err == nil {
		t.Fatal("stale resolution digest was accepted")
	}
	backup, err := ApplyPricingMigration(context.Background(), path, PricingMigrationResolutionFile{SchemaVersion: 1, MetadataSHA256: report.MetadataSHA256, Operator: "test", Resolutions: map[string]PricingMigrationResolution{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("rollback metadata missing: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if version, err := reopened.SchemaVersion(); err != nil || version != CurrentSchemaVersion() {
		t.Fatalf("version=%d err=%v", version, err)
	}
}
