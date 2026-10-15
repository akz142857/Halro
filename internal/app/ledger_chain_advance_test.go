package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// TestChainCheckpointAdvancesWhileRunning covers the window that startup-only
// reconciliation left open. The checkpoint used to move exactly once per boot,
// so every frame an instance wrote after starting sat outside it: shut the
// process down, truncate back to the offset the last start recorded, and the
// next boot reconciled cleanly against a checkpoint that had never caught up.
// On a long-lived instance that is weeks of settlements, removable without a
// trace. Advancing on shutdown (and on the usage ticker) bounds the window.
func TestChainCheckpointAdvancesWhileRunning(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	seedLedgerActivity(t, runtime)
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	// The truncation point a startup-only checkpoint would have been pinned
	// to: the state of the log as the second boot found it.
	runtime, err = Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cfg.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	sizeAtBoot := info.Size()
	request, err := runtime.accounting.BeginRequestDetailed(
		context.Background(), "project_1", "key_1", "request_2", "chat",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.accounting.Finalize(context.Background(), request, "success"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.Truncate(cfg.LedgerPath(), sizeAtBoot); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		reopened.Close()
		t.Fatal("frames written after startup must be covered by the chain checkpoint")
	}
}

// TestDerivedChainKeyIsRefusedAfterRotation pins the one case where the
// bootstrap fallback is not merely unnecessary but wrong. Rotation re-wraps
// the envelope around the same key bytes, so once it has run, deriving from
// the current Master Key produces a key that never signed anything. Silently
// deriving there turns a missing envelope into "every frame is tampered",
// which points the operator at the log instead of at the metadata database.
func TestDerivedChainKeyIsRefusedAfterRotation(t *testing.T) {
	const fingerprint = "sha256:" +
		"1111111111111111111111111111111111111111111111111111111111111111"

	openStore := func(t *testing.T, keyring *boltstore.VaultKeyring) *boltstore.Store {
		t.Helper()
		store, err := boltstore.Open(t.TempDir() + "/metadata.db")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		if keyring != nil {
			if err := store.PutVaultKeyring(*keyring); err != nil {
				t.Fatal(err)
			}
		}
		return store
	}

	// Never rotated: deriving reproduces exactly the bytes the envelope would
	// have held, so the bootstrap fallback stays available.
	if err := requireUnrotatedForDerivedKey(openStore(t, nil), "ledger"); err != nil {
		t.Fatalf("an instance with no keyring must still be allowed to derive: %v", err)
	}
	first := boltstore.VaultKeyring{FormatVersion: 1, ActiveKeyVersion: 1, ActiveFingerprint: fingerprint}
	if err := requireUnrotatedForDerivedKey(openStore(t, &first), "ledger"); err != nil {
		t.Fatalf("key version 1 must still be allowed to derive: %v", err)
	}

	// Rotated: the key inside the envelope did not change, so what derivation
	// now produces never signed anything.
	rotated := boltstore.VaultKeyring{FormatVersion: 1, ActiveKeyVersion: 2, ActiveFingerprint: fingerprint}
	if err := requireUnrotatedForDerivedKey(openStore(t, &rotated), "ledger"); err == nil {
		t.Fatal("a missing envelope must not re-derive after a Master Key rotation")
	}
}
