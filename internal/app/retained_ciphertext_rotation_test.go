package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/failurecapture"
	"github.com/akz142857/Halro/internal/masterkey"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

func TestFileMasterKeyRotationRefusesRetainedCaptureBeforeMutation(t *testing.T) {
	cfg, newKeyFile, _, _, oldKey, oldAuditKey := rotationFixture(t)
	defer clear(oldKey)
	defer clear(oldAuditKey)
	oldVault, err := vault.New(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	defer oldVault.Close()
	captures, err := failurecapture.Open(oldVault, failurecapture.Options{
		Root: filepath.Join(cfg.Storage.DataDir, "failures"), MaxBytes: 4096,
		MaxRecordsPerDay: 10, Retain: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := captures.Put(failurecapture.Record{
		RequestID: "req_rotation_guard", ProjectID: "project_rotation_guard",
		Outcome: "failed", Request: []byte(`{"prompt":"retained"}`),
	}); err != nil || !ok {
		t.Fatalf("put capture = %t, %v", ok, err)
	}

	if _, err := RotateMasterKey(context.Background(), cfg, newKeyFile); !errors.Is(err, errRetainedVaultCiphertext) {
		t.Fatalf("rotation error = %v, want retained ciphertext refusal", err)
	}
	activeKey, err := vault.LoadMasterKey(cfg.Storage.MasterKey.File)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(activeKey)
	if !bytes.Equal(activeKey, oldKey) {
		t.Fatal("refused rotation changed the active Master Key")
	}
	if _, found, err := captures.Get("req_rotation_guard", "project_rotation_guard"); err != nil || !found {
		t.Fatalf("retained capture changed after refusal: found=%t err=%v", found, err)
	}
}

func TestKMSMasterKeyRotationRefusesRetainedProviderObjectBeforeMutation(t *testing.T) {
	cfg, harness, oldKey, expectedNewKey, _, oldAuditKey := kmsRotationFixture(t)
	defer clear(oldKey)
	defer clear(expectedNewKey)
	defer clear(oldAuditKey)
	oldVault, err := vault.New(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	defer oldVault.Close()
	sealed, err := oldVault.EncryptResourceObject("response_guard:input", "project_rotation_guard", []byte("retained"))
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(cfg.Storage.DataDir, "provider-objects")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "response_guard.input"), sealed, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := rotateKMSMasterKeyWithOptions(context.Background(), cfg, kmsRotationOptions{
		factory: harness.factory, random: bytes.NewReader(expectedNewKey),
		operationID: "retained-ciphertext-guard",
	}); !errors.Is(err, errRetainedVaultCiphertext) {
		t.Fatalf("rotation error = %v, want retained ciphertext refusal", err)
	}
	metadata, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	activeKey, err := unlockKMSMasterKey(context.Background(), cfg, metadata, masterkey.KeySlotPrimary, harness.factory)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(activeKey)
	if !bytes.Equal(activeKey, oldKey) {
		t.Fatal("refused rotation changed the active KMS-backed Master Key")
	}
	if plaintext, err := oldVault.DecryptResourceObject("response_guard:input", "project_rotation_guard", sealed); err != nil || string(plaintext) != "retained" {
		t.Fatalf("retained provider object changed after refusal: %q %v", plaintext, err)
	}
}
