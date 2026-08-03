package bolt

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/masterkey"
)

type rejectingBoltCandidateVerifier struct{}

func (rejectingBoltCandidateVerifier) VerifyCandidate(context.Context, []byte) error {
	return errors.New("vault key check rejected candidate")
}

func TestStoreActivationCannotBypassCandidateVerification(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Date(2026, 8, 3, 18, 0, 0, 0, time.UTC)
	descriptor := newTestKeySlotDescriptor(t)
	if err := store.PutKeySlotDescriptor(context.Background(), descriptor); err != nil {
		t.Fatal(err)
	}
	descriptor, _, err = store.AddKeySlot(
		context.Background(), testPendingSlot("slot_primary", masterkey.KeySlotPrimary), descriptor.Revision, now,
	)
	if err != nil {
		t.Fatal(err)
	}

	key := bytes.Repeat([]byte{9}, 32)
	_, _, err = store.VerifyKeySlot(
		context.Background(), "slot_primary", descriptor.Revision, descriptor.Slots[0].Revision,
		boltTestSlotUnwrapper{key: key}, rejectingBoltCandidateVerifier{}, now.Add(time.Minute),
	)
	if err == nil || !errors.Is(err, masterkey.ErrVaultKeyMismatch) {
		t.Fatalf("candidate verification was bypassed: %v", err)
	}
	stored, err := store.KeySlotDescriptor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stored.Slots[0].State != masterkey.KeySlotPending || stored.Revision != descriptor.Revision {
		t.Fatalf("rejected activation changed persistence: %#v", stored)
	}
}

func TestPutKeySlotDescriptorRejectsNonPristineSeed(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	descriptor := newTestKeySlotDescriptor(t)
	descriptor, _, err = descriptor.AddSlot(
		testPendingSlot("slot_primary", masterkey.KeySlotPrimary), descriptor.Revision,
		time.Date(2026, 8, 3, 18, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutKeySlotDescriptor(context.Background(), descriptor); err == nil {
		t.Fatal("store accepted a non-pristine descriptor seed")
	}
}
