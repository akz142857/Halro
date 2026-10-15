package app

import (
	"context"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/usage"
	"github.com/akz142857/Halro/internal/vault"
)

// usageDump is everything the usage history index holds, read back for a
// test. Real readers page; a test's history is small enough to hold whole.
type usageDump struct {
	Totals   usage.Bucket
	Attempts []usage.AttemptEvent
	Requests []usage.RequestSummary
}

// dumpUsage brings the index level with the Ledger and reads it back.
func dumpUsage(t *testing.T, runtime *Runtime) usageDump {
	t.Helper()
	if err := runtime.usageCollector.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	dump := usageDump{Totals: runtime.usage.Totals()}
	if err := runtime.usageHistory.AttemptsAfter(0, func(attempt usage.AttemptEvent) error {
		dump.Attempts = append(dump.Attempts, attempt)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.usageHistory.RequestsAfter(0, func(summary usage.RequestSummary) error {
		dump.Requests = append(dump.Requests, summary)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return dump
}

// ledgerKeyForTest reads the Ledger key of a stopped instance.
func ledgerKeyForTest(t *testing.T, cfg config.Config) []byte {
	t.Helper()
	metadata, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	masterKey, err := vault.LoadMasterKey(cfg.Storage.MasterKey.File)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(masterKey)
	secretVault, err := vault.New(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer secretVault.Close()
	ledgerKey, err := loadLedgerHMACKey(metadata, secretVault, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	return ledgerKey
}
