package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/archive"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

func governanceRecoveryEvent(outcomeID, value, supersedes, idem string, revision uint64) governance.Event {
	now := time.Date(2026, 9, 4, 8, 0, 0, 0, time.UTC)
	idemHash := sha256.Sum256([]byte(idem))
	fingerprint := sha256.Sum256([]byte(value + supersedes))
	return governance.Event{
		EventID: "gov_" + outcomeID, ProjectID: "prj_1", WorkUnitID: "wku_unit",
		DefinitionID: "odef_result", DefinitionVersion: 1, OutcomeID: outcomeID,
		Value: value, ReporterKeyID: "key_1", ObservedAt: now, IngestedAt: now,
		SupersedesOutcomeID: supersedes, Revision: revision,
		IdempotencyKeyHash: "sha256:" + stringHex(idemHash[:]), RequestFingerprint: "sha256:" + stringHex(fingerprint[:]),
	}
}

func stringHex(value []byte) string {
	const alphabet = "0123456789abcdef"
	result := make([]byte, len(value)*2)
	for index, item := range value {
		result[index*2], result[index*2+1] = alphabet[item>>4], alphabet[item&0x0f]
	}
	return string(result)
}

func openGovernanceRecoveryFixture(t *testing.T) (*boltstore.Store, *governance.Log, string, []byte) {
	t.Helper()
	directory := t.TempDir()
	store, err := openStagedMetadataForTest(t, filepath.Join(directory, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "governance.journal")
	key := sha256.Sum256([]byte("governance recovery test key"))
	log, err := governance.Open(path, key[:])
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	return store, log, path, key[:]
}

// governanceTestArchive is an empty archive for a restore to fill.
func governanceTestArchive(t *testing.T) *archive.Store {
	t.Helper()
	store, _, err := archive.Open(filepath.Join(t.TempDir(), "archive.db"), bytes.Repeat([]byte{0x44}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestGovernanceAnchorRejectsCompleteSuffixTruncation(t *testing.T) {
	store, log, path, key := openGovernanceRecoveryFixture(t)
	defer store.Close()
	if _, err := log.Append(context.Background(), governanceRecoveryEvent("out_first", "accepted", "", "one", 1)); err != nil {
		t.Fatal(err)
	}
	firstEnd := log.Summary().Bytes
	if _, err := log.Append(context.Background(), governanceRecoveryEvent("out_second", "rejected", "out_first", "two", 2)); err != nil {
		t.Fatal(err)
	}
	head, authentication := log.AuthenticatedHead()
	if err := store.PutGovernanceJournalAnchor(head.Records, head.Bytes, head.LastHash, authentication); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, firstEnd); err != nil {
		t.Fatal(err)
	}
	reopened, err := governance.Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := restoreGovernanceState(store, reopened, key, governanceTestArchive(t)); !errors.Is(err, errGovernanceAnchorMismatch) {
		t.Fatalf("complete suffix truncation error=%v", err)
	}
}

func TestGovernanceNonEmptyJournalRequiresAnchor(t *testing.T) {
	store, log, _, key := openGovernanceRecoveryFixture(t)
	defer store.Close()
	defer log.Close()
	if _, err := log.Append(context.Background(), governanceRecoveryEvent("out_first", "accepted", "", "one", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreGovernanceState(store, log, key, governanceTestArchive(t)); !errors.Is(err, errGovernanceAnchorMismatch) {
		t.Fatalf("non-empty unanchored journal error=%v", err)
	}
}

func TestGovernanceAnchorAllowsRepairAfterUnconfirmedPartialTail(t *testing.T) {
	store, log, path, key := openGovernanceRecoveryFixture(t)
	defer store.Close()
	if _, err := log.Append(context.Background(), governanceRecoveryEvent("out_first", "accepted", "", "one", 1)); err != nil {
		t.Fatal(err)
	}
	head, authentication := log.AuthenticatedHead()
	if err := store.PutGovernanceJournalAnchor(head.Records, head.Bytes, head.LastHash, authentication); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(context.Background(), governanceRecoveryEvent("out_second", "rejected", "out_first", "two", 2)); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, head.Bytes+10); err != nil {
		t.Fatal(err)
	}
	reopened, err := governance.Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state, err := restoreGovernanceState(store, reopened, key, governanceTestArchive(t))
	if err != nil {
		t.Fatal(err)
	}
	if state.Sequence() != 1 || reopened.Summary().Bytes != head.Bytes {
		t.Fatalf("state sequence=%d journal bytes=%d, want confirmed head %+v", state.Sequence(), reopened.Summary().Bytes, head)
	}
}

func TestGovernanceCheckpointAuthenticationFailureFallsBackToJournal(t *testing.T) {
	store, log, _, key := openGovernanceRecoveryFixture(t)
	defer store.Close()
	defer log.Close()
	event := governanceRecoveryEvent("out_actual", "accepted", "", "actual", 1)
	if _, err := log.Append(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	head, anchorAuthentication := log.AuthenticatedHead()
	if err := store.PutGovernanceJournalAnchor(head.Records, head.Bytes, head.LastHash, anchorAuthentication); err != nil {
		t.Fatal(err)
	}
	forgedOutcome := domain.Outcome{ID: "out_forged", ProjectID: "prj_1", WorkUnitID: "wku_unit", DefinitionID: "odef_result", DefinitionVersion: 1,
		Value: "rejected", ReporterKeyID: "key_1", ObservedAt: event.ObservedAt, IngestedAt: event.IngestedAt, Revision: 1, GovernanceSequence: 1}
	forged := []governance.SnapshotRecord{{Outcome: forgedOutcome, IdempotencyKeyHash: event.IdempotencyKeyHash, RequestFingerprint: event.RequestFingerprint}}
	payload, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	badAuthentication := sha256.Sum256([]byte("not a valid checkpoint authentication"))
	if err := store.SaveGovernanceCheckpoint(1, head.Bytes, head.LastHash, sha256.Sum256(payload), badAuthentication, payload); err != nil {
		t.Fatal(err)
	}
	state, err := restoreGovernanceState(store, log, key, governanceTestArchive(t))
	if err != nil {
		t.Fatal(err)
	}
	actual, ok, err := state.Current("prj_1", "wku_unit", "odef_result", 1)
	if err != nil || !ok || actual.ID != "out_actual" || actual.Value != "accepted" {
		t.Fatalf("restored outcome=%#v ok=%v", actual, ok)
	}
	if _, err := store.LoadGovernanceCheckpoint(); !errors.Is(err, boltstore.ErrNotFound) {
		t.Fatalf("unauthenticated checkpoint was not discarded: %v", err)
	}
}

func TestGovernanceCheckpointMustMatchItsJournalFrame(t *testing.T) {
	store, log, _, key := openGovernanceRecoveryFixture(t)
	defer store.Close()
	defer log.Close()
	event := governanceRecoveryEvent("out_actual", "accepted", "", "actual", 1)
	if _, err := log.Append(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	head, anchorAuthentication := log.AuthenticatedHead()
	if err := store.PutGovernanceJournalAnchor(head.Records, head.Bytes, head.LastHash, anchorAuthentication); err != nil {
		t.Fatal(err)
	}
	forgedOutcome := domain.Outcome{ID: "out_forged", ProjectID: "prj_1", WorkUnitID: "wku_unit", DefinitionID: "odef_result", DefinitionVersion: 1,
		Value: "rejected", ReporterKeyID: "key_1", ObservedAt: event.ObservedAt, IngestedAt: event.IngestedAt, Revision: 1, GovernanceSequence: 1}
	forged := []governance.SnapshotRecord{{Outcome: forgedOutcome, IdempotencyKeyHash: event.IdempotencyKeyHash, RequestFingerprint: event.RequestFingerprint}}
	payload, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	payloadHash := sha256.Sum256(payload)
	wrongJournalHash := sha256.Sum256([]byte("a different journal frame"))
	authentication := governance.CheckpointAuth(key, 1, head.Bytes, wrongJournalHash, payloadHash)
	if err := store.SaveGovernanceCheckpoint(1, head.Bytes, wrongJournalHash, payloadHash, authentication, payload); err != nil {
		t.Fatal(err)
	}
	state, err := restoreGovernanceState(store, log, key, governanceTestArchive(t))
	if err != nil {
		t.Fatal(err)
	}
	actual, ok, err := state.Current("prj_1", "wku_unit", "odef_result", 1)
	if err != nil || !ok || actual.ID != "out_actual" || actual.Value != "accepted" {
		t.Fatalf("restored outcome=%#v ok=%v", actual, ok)
	}
	if _, err := store.LoadGovernanceCheckpoint(); !errors.Is(err, boltstore.ErrNotFound) {
		t.Fatalf("journal-mismatched checkpoint was not discarded: %v", err)
	}
}

func governanceBackupFixture(t *testing.T) (configPath string, cfg config.Config, store *boltstore.Store, log *governance.Log, key []byte) {
	t.Helper()
	cfg = testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	var err error
	store, err = openMetadataForTest(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	masterKey, err := unlockMasterKey(context.Background(), cfg, store)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	secretVault, err := vault.New(masterKey)
	if err != nil {
		clear(masterKey)
		store.Close()
		t.Fatal(err)
	}
	ledgerKey, err := loadLedgerHMACKey(store, secretVault, masterKey)
	secretVault.Close()
	clear(masterKey)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	key, err = vault.DeriveGovernanceHMACKey(ledgerKey)
	clear(ledgerKey)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	log, err = governance.Open(cfg.GovernancePath(), key)
	if err != nil {
		clear(key)
		store.Close()
		t.Fatal(err)
	}
	configPath = filepath.Join(filepath.Dir(cfg.Storage.DataDir), "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0o600); err != nil {
		log.Close()
		clear(key)
		store.Close()
		t.Fatal(err)
	}
	return configPath, cfg, store, log, key
}

func TestBackupVerifiesGovernanceTerminalAnchorBeforePublication(t *testing.T) {
	t.Run("missing anchor", func(t *testing.T) {
		configPath, cfg, store, log, key := governanceBackupFixture(t)
		if _, err := log.Append(context.Background(), governanceRecoveryEvent("out_first", "accepted", "", "one", 1)); err != nil {
			t.Fatal(err)
		}
		log.Close()
		store.Close()
		clear(key)
		output := filepath.Join(filepath.Dir(cfg.Storage.DataDir), "missing-anchor.hmbk")
		if _, err := CreateBackup(context.Background(), cfg, configPath, output, bytes.Repeat([]byte{0x41}, 32)); !errors.Is(err, errGovernanceAnchorMismatch) {
			t.Fatalf("missing anchor backup error=%v", err)
		}
		if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid backup was published: %v", err)
		}
	})

	t.Run("invalid authentication", func(t *testing.T) {
		configPath, cfg, store, log, key := governanceBackupFixture(t)
		if _, err := log.Append(context.Background(), governanceRecoveryEvent("out_first", "accepted", "", "one", 1)); err != nil {
			t.Fatal(err)
		}
		head := log.Summary()
		badAuthentication := sha256.Sum256([]byte("invalid anchor authentication"))
		if err := store.PutGovernanceJournalAnchor(head.Records, head.Bytes, head.LastHash, badAuthentication); err != nil {
			t.Fatal(err)
		}
		log.Close()
		store.Close()
		clear(key)
		output := filepath.Join(filepath.Dir(cfg.Storage.DataDir), "bad-anchor.hmbk")
		if _, err := CreateBackup(context.Background(), cfg, configPath, output, bytes.Repeat([]byte{0x42}, 32)); !errors.Is(err, errGovernanceAnchorMismatch) {
			t.Fatalf("invalid anchor authentication backup error=%v", err)
		}
	})

	t.Run("anchor frame mismatch", func(t *testing.T) {
		configPath, cfg, store, log, key := governanceBackupFixture(t)
		if _, err := log.Append(context.Background(), governanceRecoveryEvent("out_first", "accepted", "", "one", 1)); err != nil {
			t.Fatal(err)
		}
		head := log.Summary()
		wrongHash := sha256.Sum256([]byte("wrong authenticated frame"))
		authentication := governance.JournalAnchorAuth(key, head.Records, head.Bytes, wrongHash)
		if err := store.PutGovernanceJournalAnchor(head.Records, head.Bytes, wrongHash, authentication); err != nil {
			t.Fatal(err)
		}
		log.Close()
		store.Close()
		clear(key)
		output := filepath.Join(filepath.Dir(cfg.Storage.DataDir), "mismatched-anchor.hmbk")
		if _, err := CreateBackup(context.Background(), cfg, configPath, output, bytes.Repeat([]byte{0x43}, 32)); !errors.Is(err, errGovernanceAnchorMismatch) {
			t.Fatalf("mismatched anchor backup error=%v", err)
		}
	})

	t.Run("anchor ahead after truncation", func(t *testing.T) {
		configPath, cfg, store, log, key := governanceBackupFixture(t)
		if _, err := log.Append(context.Background(), governanceRecoveryEvent("out_first", "accepted", "", "one", 1)); err != nil {
			t.Fatal(err)
		}
		head, authentication := log.AuthenticatedHead()
		if err := store.PutGovernanceJournalAnchor(head.Records, head.Bytes, head.LastHash, authentication); err != nil {
			t.Fatal(err)
		}
		log.Close()
		store.Close()
		clear(key)
		if err := os.Truncate(cfg.GovernancePath(), 0); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(filepath.Dir(cfg.Storage.DataDir), "truncated-anchor.hmbk")
		if _, err := CreateBackup(context.Background(), cfg, configPath, output, bytes.Repeat([]byte{0x45}, 32)); !errors.Is(err, errGovernanceAnchorMismatch) {
			t.Fatalf("anchor-ahead backup error=%v", err)
		}
	})

	t.Run("valid lagging anchor", func(t *testing.T) {
		configPath, cfg, store, log, key := governanceBackupFixture(t)
		if _, err := log.Append(context.Background(), governanceRecoveryEvent("out_first", "accepted", "", "one", 1)); err != nil {
			t.Fatal(err)
		}
		head, authentication := log.AuthenticatedHead()
		if err := store.PutGovernanceJournalAnchor(head.Records, head.Bytes, head.LastHash, authentication); err != nil {
			t.Fatal(err)
		}
		if _, err := log.Append(context.Background(), governanceRecoveryEvent("out_second", "rejected", "out_first", "two", 2)); err != nil {
			t.Fatal(err)
		}
		log.Close()
		store.Close()
		clear(key)
		output := filepath.Join(filepath.Dir(cfg.Storage.DataDir), "lagging-anchor.hmbk")
		manifest, err := CreateBackup(context.Background(), cfg, configPath, output, bytes.Repeat([]byte{0x44}, 32))
		if err != nil {
			t.Fatal(err)
		}
		if manifest.GovernanceSequence != 2 {
			t.Fatalf("backup governance sequence=%d, want 2", manifest.GovernanceSequence)
		}
	})
}

// An archive written from another journal — same position, another record
// there — is emptied and filled again from this journal, so it serves none of
// the other journal's Outcomes.
func TestGovernanceArchiveFromAnotherJournalIsRefilled(t *testing.T) {
	previous := outcomeFlushThreshold
	outcomeFlushThreshold = 1
	defer func() { outcomeFlushThreshold = previous }()
	shared := governanceTestArchive(t)
	restoreWith := func(outcomeID, value string) (*governance.State, *governance.Log) {
		t.Helper()
		store, log, _, key := openGovernanceRecoveryFixture(t)
		t.Cleanup(func() { store.Close() })
		if _, err := log.Append(context.Background(), governanceRecoveryEvent(outcomeID, value, "", outcomeID, 1)); err != nil {
			t.Fatal(err)
		}
		head, authentication := log.AuthenticatedHead()
		if err := store.PutGovernanceJournalAnchor(head.Records, head.Bytes, head.LastHash, authentication); err != nil {
			t.Fatal(err)
		}
		state, err := restoreGovernanceState(store, log, key, shared)
		if err != nil {
			t.Fatal(err)
		}
		return state, log
	}
	_, first := restoreWith("out_other", "rejected")
	first.Close()
	if _, found, err := shared.ArchivedOutcome("out_other"); err != nil || !found {
		t.Fatalf("the first journal's Outcome was not archived: found=%t err=%v", found, err)
	}
	state, second := restoreWith("out_actual", "accepted")
	defer second.Close()
	if _, found, err := shared.ArchivedOutcome("out_other"); err != nil || found {
		t.Fatalf("the other journal's Outcome survived: found=%t err=%v", found, err)
	}
	current, found, err := state.Current("prj_1", "wku_unit", "odef_result", 1)
	if err != nil || !found || current.ID != "out_actual" {
		t.Fatalf("head %#v found=%t err=%v", current, found, err)
	}
	if through, hash, err := shared.OutcomesThrough(); err != nil || through != 1 || hash != second.Summary().LastHash {
		t.Fatalf("archive through=%d err=%v", through, err)
	}
}
