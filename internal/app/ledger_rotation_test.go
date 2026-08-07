package app

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// TestMasterKeyRotationKeepsEarlierLedgerFramesVerifiable is the invariant the
// whole envelope design exists for, and the one thing it had no end-to-end test
// of. ADR 0016's reason for storing the chain key in a Vault envelope rather
// than deriving it per open is that rotation would otherwise invalidate every
// frame ever written: rotation re-wraps the envelope around the same key bytes,
// so history stays verifiable. internal/ledger's rotation test says of itself
// that it simulates this at the ledger layer — it reopens with the same key and
// never touches vault, key_rotation.go, or the envelope. Nothing exercised the
// real path, which is where the mistake would be.
func TestMasterKeyRotationKeepsEarlierLedgerFramesVerifiable(t *testing.T) {
	cfg, newKeyFile, _, _, oldKey, oldAuditKey := rotationFixture(t)
	defer clear(oldKey)
	defer clear(oldAuditKey)

	// Frames written before the rotation, under the pre-rotation Master Key.
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	seedLedgerActivity(t, runtime)
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := VerifyLedger(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if before.Authenticated == 0 {
		t.Fatal("expected authenticated frames before the rotation")
	}

	if _, err := RotateMasterKey(context.Background(), cfg, newKeyFile); err != nil {
		t.Fatal(err)
	}

	// The same frames, read under the new Master Key. Deriving the chain key
	// from the Master Key would produce a different one here and report every
	// frame as tampered.
	after, err := VerifyLedger(context.Background(), cfg)
	if err != nil {
		t.Fatalf("frames written before the rotation no longer verify: %v", err)
	}
	if after.Authenticated != before.Authenticated || after.ChainHash != before.ChainHash {
		t.Fatalf("chain changed across rotation: authenticated %d -> %d",
			before.Authenticated, after.Authenticated)
	}

	// And the instance still starts and appends, so the rotation left a log the
	// running gateway can extend rather than only one an offline tool can read.
	reopened, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("reopen after rotation: %v", err)
	}
	seedLedgerActivity(t, reopened)
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	extended, err := VerifyLedger(context.Background(), cfg)
	if err != nil {
		t.Fatalf("frames appended after the rotation do not verify: %v", err)
	}
	if extended.Authenticated <= before.Authenticated {
		t.Fatalf("appending after rotation produced no new authenticated frames: %d -> %d",
			before.Authenticated, extended.Authenticated)
	}
	var zero [32]byte
	if extended.ChainHash == zero {
		t.Fatal("the chain restarted from zero after rotation instead of continuing")
	}
}
