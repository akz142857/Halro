package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// Restoring an archive taken before a compromise response puts the disabled key
// back. Nothing in the archive can know that, so the control is that restore
// names every Gateway Key whose enabled flag it just restored, and
// docs/guides/backup-restore.md makes comparing that list against the
// revocation record a mandatory step before accepting traffic.
//
// That list is the whole control, and the case it exists for — a key disabled
// after the backup was taken — had no test: the existing coverage restores a
// key that was enabled the whole time, which the same code would report even if
// it ignored the archive entirely.
func TestRestoreNamesAGatewayKeyThatWasDisabledAfterTheBackup(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test",
		PublicModel: "chat", ProjectName: "Revocation", BillingMode: domain.BillingModeFree,
	}, []byte("revocation-provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(cfg.Storage.DataDir)
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A second key that is already disabled when the backup is taken. It is the
	// control: restore must not name it, because restoring it changes nothing.
	const disabledKeyID = "gwk_disabled_at_backup_time"
	seedStore, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	live, err := seedStore.GetGatewayKey(context.Background(), bootstrap.KeyID)
	if err != nil {
		seedStore.Close()
		t.Fatal(err)
	}
	disabled := live
	disabled.ID = disabledKeyID
	disabled.Name = "disabled at backup time"
	disabled.Enabled = false
	disabled.Revision = 0
	disabled.KeyHash = [32]byte{0x01}
	if _, err := seedStore.PutGatewayKey(context.Background(), disabled, 0, nil); err != nil {
		seedStore.Close()
		t.Fatal(err)
	}
	if err := seedStore.Close(); err != nil {
		t.Fatal(err)
	}

	backupKey := bytes.Repeat([]byte{0x5a}, 32)
	archivePath := filepath.Join(root, "before-revocation.hmbk")
	manifest, err := CreateBackup(context.Background(), cfg, configPath, archivePath, backupKey)
	if err != nil {
		t.Fatal(err)
	}

	// Flip both keys after the backup: the one that was enabled is disabled, the
	// one that was disabled is enabled. A report that simply echoed live state,
	// or that always named every key it saw, would name both or the wrong one —
	// only a report computed from the archive's enabled set names A alone.
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := setGatewayKeyEnabled(t, store, bootstrap.KeyID, false); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := setGatewayKeyEnabled(t, store, disabledKeyID, true); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := RestoreBackup(context.Background(), cfg, archivePath, backupKey, manifest.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	if result.RestoredEnabledGatewayKeyCount != 1 ||
		!slices.Equal(result.RestoredEnabledGatewayKeyIDs, []string{bootstrap.KeyID}) {
		t.Fatalf("restore did not name the re-enabled key: count=%d ids=%v",
			result.RestoredEnabledGatewayKeyCount, result.RestoredEnabledGatewayKeyIDs)
	}

	// And the key really is back, which is why the operator has to act on that
	// list rather than read it as a formality. The control key goes the other
	// way: restore disables it again, and that is not what the list reports on.
	restored, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	revived, err := restored.GetGatewayKey(context.Background(), bootstrap.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	if !revived.Enabled {
		t.Fatal("restore left the key disabled; the reported list no longer describes what restore does")
	}
	control, err := restored.GetGatewayKey(context.Background(), disabledKeyID)
	if err != nil {
		t.Fatal(err)
	}
	if control.Enabled {
		t.Fatal("restore left the control key enabled; the archive's enabled set is not what was applied")
	}
}

func setGatewayKeyEnabled(t *testing.T, store *boltstore.Store, id string, enabled bool) error {
	t.Helper()
	key, err := store.GetGatewayKey(context.Background(), id)
	if err != nil {
		return err
	}
	key.Enabled = enabled
	_, err = store.PutGatewayKey(context.Background(), key, key.Revision, nil)
	return err
}
