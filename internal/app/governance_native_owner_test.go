package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/vault"
)

func TestStandaloneGovernanceFactoryRejectsNativeWriterDetachedFromIdenticalSource(t *testing.T) {
	r, _, _ := openBootstrappedRuntime(t)
	t.Cleanup(func() { r.Close() })
	ctx := context.Background()
	if err := r.governance.manager.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.governance.owner.close(); err != nil {
		t.Fatal(err)
	}
	path := r.config.GovernancePath()
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".captured"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	master, err := unlockMasterKey(ctx, r.config)
	if err != nil {
		t.Fatal(err)
	}
	ledgerKey, err := loadLedgerHMACKey(r.store, r.vault, master)
	clear(master)
	if err != nil {
		t.Fatal(err)
	}
	key, err := vault.DeriveGovernanceHMACKey(ledgerKey)
	clear(ledgerKey)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	owner, err := prepareStandaloneGovernance(ctx, r.config, r.store, r.governance.log, key, r.resourcePressure)
	if owner != nil {
		defer owner.close()
	}
	if owner != nil || !errors.Is(err, governance.ErrCorrupt) {
		t.Fatal("detached native writer accepted", owner, err)
	}
	if _, err := os.Stat(standaloneGovernanceProjectionPath(r.config)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("candidate created before native owner qualification", err)
	}
	for _, name := range []string{path, path + ".captured"} {
		got, err := os.ReadFile(name)
		if err != nil || !bytes.Equal(got, original) {
			t.Fatal("native evidence changed", name, err)
		}
	}
}
