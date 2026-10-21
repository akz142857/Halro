package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// journalledInstance is an initialised, bootstrapped data directory: enough
// authoritative metadata that the journal has something to describe.
func journalledInstance(t *testing.T) (config.Config, BootstrapResult) {
	t.Helper()
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	result, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test",
		PublicModel: "chat", ProjectName: "Journal", BillingMode: domain.BillingModeFree,
	}, []byte("provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg, result
}

// TestInitializePublishesEpochOneAndRecordsWhatFollows.
//
// The database as it stands when the journal is attached is epoch 1's starting
// projection; everything after that point is an operation inside it. Bootstrap
// writes a Credential, Provider, Deployment, Route, Project and Gateway Key, so
// the file has to have grown.
func TestInitializePublishesEpochOneAndRecordsWhatFollows(t *testing.T) {
	cfg, _ := journalledInstance(t)
	store, err := openMetadataForTest(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, err := store.MetadataJournalState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Epoch != 1 {
		t.Fatalf("a fresh instance is at epoch %d", state.Epoch)
	}
	if state.Sequence == 0 || state.Applied != state.Sequence {
		t.Fatalf("bootstrap recorded nothing, or the projection lags: %+v", state)
	}
	if _, err := os.Stat(filepath.Join(cfg.Storage.DataDir, boltstore.MetadataJournalFileName)); err != nil {
		t.Fatalf("the journal is not beside the database: %v", err)
	}
}

// TestTheJournalCarriesNoCallerContent is the invariant that decides whether
// this file may exist at all. Frames are unencrypted, so their data
// classification has to be bbolt's own: AEAD ciphertext for Credentials,
// Argon2id hashes for passwords, and nothing a caller wrote.
func TestTheJournalCarriesNoCallerContent(t *testing.T) {
	cfg, _ := journalledInstance(t)
	store, err := openMetadataForTest(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := store.MetadataJournalPath()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The Provider secret bootstrap was given. It is sealed in bbolt and must
	// be just as sealed here, because the recorder copies the bytes bbolt
	// stores rather than the values a caller supplied.
	if strings.Contains(string(raw), "provider-secret") {
		t.Fatal("the journal carries a Provider secret in the clear")
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("journal permissions are %04o", info.Mode().Perm())
	}
}

// TestRestoreWithdrawsTheArchivedJournalAndOpensANewEpoch.
//
// A restore is a whole-file publish of the projection, so the chain the archive
// carries stops describing the database the moment it is published. Leaving the
// old file in place would let a reader continue a chain that no longer matches
// what it projects (§6.1.4, §12.2).
func TestRestoreWithdrawsTheArchivedJournalAndOpensANewEpoch(t *testing.T) {
	cfg, _ := journalledInstance(t)
	archive, configPath, backupKey := journalBackupInputs(t, cfg)
	if _, err := CreateBackup(context.Background(), cfg, configPath, archive, backupKey); err != nil {
		t.Fatal(err)
	}
	before, err := openMetadataForTest(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	beforeState, err := before.MetadataJournalState()
	if err != nil {
		t.Fatal(err)
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	manifest, err := VerifyBackup(archive, backupKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreBackup(context.Background(), cfg, archive, backupKey, manifest.BackupID); err != nil {
		t.Fatal(err)
	}
	// The restored directory carries a journal — its own, at the next epoch.
	// It is published with the database rather than left to the first start,
	// so there is never a directory following an epoch whose file is absent.
	journalPath := filepath.Join(cfg.Storage.DataDir, boltstore.MetadataJournalFileName)
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("the restored directory has no journal: %v", err)
	}
	report, err := Doctor(context.Background(), cfg)
	if err != nil || !report.Healthy {
		t.Fatalf("a freshly restored directory does not pass doctor: healthy=%v err=%v", report.Healthy, err)
	}
	after, err := openMetadataForTest(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	afterState, err := after.MetadataJournalState()
	if err != nil {
		t.Fatal(err)
	}
	if afterState.Epoch <= beforeState.Epoch {
		t.Fatalf("a restore continued epoch %d instead of opening the next one", afterState.Epoch)
	}
}

// TestTheBackupManifestRecordsWhichPrefixItProjects.
//
// A restored database is a projection, and a manifest that did not say which
// journal prefix it projects could only be taken on trust (§12.1).
func TestTheBackupManifestRecordsWhichPrefixItProjects(t *testing.T) {
	cfg, _ := journalledInstance(t)
	archive, configPath, backupKey := journalBackupInputs(t, cfg)
	manifest, err := CreateBackup(context.Background(), cfg, configPath, archive, backupKey)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Metadata.MetadataJournalEpoch != 1 {
		t.Fatalf("the manifest records epoch %d", manifest.Metadata.MetadataJournalEpoch)
	}
	if manifest.Metadata.MetadataJournalSequence == 0 {
		t.Fatal("the manifest records no journal position")
	}
	var carried bool
	for _, file := range manifest.Files {
		if file.Path == "data/"+boltstore.MetadataJournalFileName {
			carried = true
		}
	}
	if !carried {
		t.Fatalf("the archive does not carry the journal: %+v", manifest.Files)
	}
}

// TestDoctorReportsTheJournalAgainstItsProjection. `halro doctor` is the only
// command that writes nothing at all, which makes it the one place a divergence
// can be seen without first risking a repair.
func TestDoctorReportsTheJournalAgainstItsProjection(t *testing.T) {
	cfg, _ := journalledInstance(t)
	report, err := Doctor(context.Background(), cfg)
	if err != nil || !report.Healthy {
		t.Fatalf("healthy doctor report=%#v err=%v", report, err)
	}
	var found bool
	for _, check := range report.Checks {
		if check.Name != "metadata_journal" {
			continue
		}
		found = true
		if check.Status != "pass" || !strings.Contains(check.Detail, "epoch 1") {
			t.Fatalf("journal check is %q: %s", check.Status, check.Detail)
		}
	}
	if !found {
		t.Fatal("doctor does not report the metadata journal")
	}
}

// TestDoctorNamesADivergenceRatherThanRepairingIt. Doctor is read-only, so a
// projection ahead of its log has to be reported and left alone — repairing it
// would mean choosing between discarding recorded writes and overwriting newer
// state, which is exactly the choice nothing in this process can make.
func TestDoctorNamesADivergenceRatherThanRepairingIt(t *testing.T) {
	cfg, _ := journalledInstance(t)
	journalPath := filepath.Join(cfg.Storage.DataDir, boltstore.MetadataJournalFileName)
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte inside the first frame's payload.
	corrupted := append([]byte(nil), before...)
	corrupted[len(corrupted)/2] ^= 0xff
	if err := os.WriteFile(journalPath, corrupted, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Doctor(context.Background(), cfg)
	if err == nil || report.Healthy {
		t.Fatalf("a corrupt journal passed doctor: %#v", report)
	}
	var reported bool
	for _, check := range report.Checks {
		if check.Name == "metadata_journal" && check.Status == "fail" {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("doctor did not name the journal: %#v", report.Checks)
	}
	after, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(corrupted) {
		t.Fatal("doctor repaired the journal it was asked to inspect")
	}
}

// TestAnInstanceThatPredatesTheJournalStartsAndSealsItsOwnKey.
//
// The upgrade path. A data directory written before this existed has no journal
// and no envelope; the attach derives a key, publishes epoch 1 with the
// database as its starting projection, and seals the envelope through the entry
// it just opened.
func TestAnInstanceThatPredatesTheJournalStartsAndSealsItsOwnKey(t *testing.T) {
	cfg, _ := journalledInstance(t)
	journalPath := filepath.Join(cfg.Storage.DataDir, boltstore.MetadataJournalFileName)
	if err := os.Remove(journalPath); err != nil {
		t.Fatal(err)
	}
	// And remove the envelope, which such a directory would not have either.
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteMetadataJournalHMACEnvelopeForTest(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openMetadataForTest(t, cfg)
	if err != nil {
		t.Fatalf("an instance predating the journal did not open: %v", err)
	}
	defer reopened.Close()
	state, err := reopened.MetadataJournalState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Epoch != 2 {
		t.Fatalf("the upgrade opened epoch %d, want the next one after 1", state.Epoch)
	}
	envelope, err := reopened.MetadataJournalHMACEnvelope()
	if err != nil || len(envelope) == 0 {
		t.Fatalf("the attach did not seal a key: %v", err)
	}
	// And the file it published authenticates under the sealed key.
	if _, err := metadatajournal.Verify(state.Path, mustJournalKey(t, cfg, reopened)); err != nil {
		t.Fatal(err)
	}
}

func mustJournalKey(t *testing.T, cfg config.Config, store *boltstore.Store) []byte {
	t.Helper()
	key, err := testJournalKeyFor(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// journalBackupInputs stages the three things CreateBackup needs.
func journalBackupInputs(t *testing.T, cfg config.Config) (archive, configPath string, backupKey []byte) {
	t.Helper()
	root := filepath.Dir(cfg.Storage.DataDir)
	configPath = filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "journal-backup.hmbk"), configPath, bytes.Repeat([]byte{0x4d}, 32)
}
