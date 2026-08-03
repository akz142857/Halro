package app

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/masterkey"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
)

type appTestSlotUnwrapper struct {
	key []byte
}

func (u *appTestSlotUnwrapper) Unwrap(context.Context, masterkey.KeySlot) ([]byte, error) {
	return u.key, nil
}

func TestKeySlotVerificationRejectsValidKeyFromAnotherVault(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// This is a structurally valid Master Key, and the descriptor fingerprint
	// deliberately matches it. It must still fail because it cannot decrypt the
	// current metadata store's Vault Key Check.
	wrongVaultKey := bytes.Repeat([]byte{0x5a}, 32)
	fingerprint, err := masterkey.MasterKeyFingerprint(wrongVaultKey)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := masterkey.NewKeySlotDescriptor(fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 3, 16, 0, 0, 0, time.UTC)
	descriptor, _, err = descriptor.AddSlot(masterkey.PendingKeySlot{
		ID: "wrong_vault", Provider: "fake-kms", Purpose: masterkey.KeySlotPrimary,
		KeyReference: "opaque-reference", Algorithm: "fake-v1", WrappedKey: []byte("wrapped-wrong-key"),
	}, descriptor.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	unwrapper := &appTestSlotUnwrapper{key: bytes.Clone(wrongVaultKey)}
	next, transition, err := descriptor.VerifySlot(
		context.Background(), "wrong_vault", descriptor.Revision, 1,
		unwrapper, vaultCandidateVerifier{store: store}, now.Add(time.Minute),
	)
	if !errors.Is(err, masterkey.ErrVaultKeyMismatch) {
		t.Fatalf("wrong Vault error=%v", err)
	}
	if transition != nil || next.Revision != 0 {
		t.Fatalf("wrong Vault candidate changed state: next=%#v transition=%#v", next, transition)
	}
	if !allZeroAppKey(unwrapper.key) {
		t.Fatal("candidate Master Key was not cleared")
	}
	if descriptor.Slots[0].State != masterkey.KeySlotPending || descriptor.Slots[0].VerifiedAt != nil {
		t.Fatalf("source descriptor changed after rejection: %#v", descriptor.Slots[0])
	}
}

func allZeroAppKey(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}
