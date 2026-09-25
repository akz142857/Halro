package app

import (
	"context"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/metadatajournal"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

// openMetadataForTest is boltstore.Open plus the journal attach every
// authoritative write now requires.
//
// It takes the configuration rather than a path because the key matters: a
// database this process wrote through the derived journal key cannot be
// reopened with any other, so the helper unwraps the Master Key exactly the way
// production does.
func openMetadataForTest(tb testing.TB, cfg config.Config) (*boltstore.Store, error) {
	tb.Helper()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return nil, err
	}
	masterKey, err := unlockMasterKey(context.Background(), cfg, store)
	if err != nil {
		// No usable Master Key yet — a directory that was never initialised,
		// or one whose key material this fixture does not hold. Such a caller
		// is reading; one that writes gets ErrJournalUnavailable, which is the
		// honest answer rather than a silently unrecorded write.
		return store, nil
	}
	defer clear(masterKey)
	secretVault, err := vault.New(masterKey)
	if err != nil {
		store.Close()
		return nil, err
	}
	defer secretVault.Close()
	if _, err := attachMetadataJournal(store, secretVault, masterKey, "test"); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

// openMetadataReadForTest opens the metadata database without attaching a
// journal, for a test that only reads it — including the KMS fixtures, where
// unwrapping the Master Key would consume a scripted provider call and change
// what the test is measuring.
func openMetadataReadForTest(tb testing.TB, cfg config.Config) (*boltstore.Store, error) {
	tb.Helper()
	return boltstore.Open(cfg.MetadataPath())
}

// openStagedMetadataForTest opens a database by path, with no configuration
// behind it.
//
// It attaches a journal only to a database that has none — a bare bbolt file a
// test staged itself, where a fixed key is both sufficient and stable across
// reopens. A database production already wrote carries a journal signed with a
// derived key this helper cannot reproduce, and every such caller here is
// reading rather than writing; one that writes gets ErrJournalUnavailable,
// which is the honest answer rather than a silently unrecorded write.
func openStagedMetadataForTest(tb testing.TB, path string) (*boltstore.Store, error) {
	tb.Helper()
	store, err := boltstore.Open(path)
	if err != nil {
		return nil, err
	}
	state, err := store.MetadataJournalState()
	if err != nil {
		store.Close()
		return nil, err
	}
	if state.Epoch > 0 {
		return store, nil
	}
	key := fixedTestJournalKey()
	defer clear(key)
	if _, err := store.AttachMetadataJournal(key, "test"); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

func fixedTestJournalKey() []byte {
	key := make([]byte, metadatajournal.KeySize)
	for index := range key {
		key[index] = 0x5c
	}
	return key
}

// testJournalKeyFor derives the journal key the way production does, for a
// test that needs to authenticate the file itself.
func testJournalKeyFor(cfg config.Config, store *boltstore.Store) ([]byte, error) {
	masterKey, err := unlockMasterKey(context.Background(), cfg, store)
	if err != nil {
		return nil, err
	}
	defer clear(masterKey)
	secretVault, err := vault.New(masterKey)
	if err != nil {
		return nil, err
	}
	defer secretVault.Close()
	return loadMetadataJournalHMACKey(store, secretVault, masterKey)
}
