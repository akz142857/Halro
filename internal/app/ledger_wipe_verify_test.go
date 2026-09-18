package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	bbolt "go.etcd.io/bbolt"
)

// The wipe an operator runs `ledger verify` to catch. Truncating the WAL to
// zero leaves the file a fresh `init` leaves, and the bbolt chain checkpoint —
// plain JSON, authenticated by nothing — can be overwritten with the same zero
// value `init` seeds, so the resulting report is byte-identical to a new
// install's. Nothing here needs the master key or the chain HMAC key: write
// access to the data directory is the whole capability.
//
// VerifyLedger cannot tell the two apart and returns a report rather than an
// error, which is exactly why the command above it treats an unauthenticated
// chain as a failure whether or not it holds frames.
func TestVerifyLedgerReportsNoAuthenticatedChainAfterAWipedWALAndZeroedCheckpoint(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	billOneRequest(t, runtime, "wipe_target")
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	billed, err := VerifyLedger(context.Background(), cfg)
	if err != nil || !billed.ChainVerified {
		t.Fatalf("billed ledger did not verify: report=%#v err=%v", billed, err)
	}

	if err := os.Truncate(cfg.LedgerPath(), 0); err != nil {
		t.Fatal(err)
	}
	// Truncation alone is caught by the checkpoint, so the wipe has to take
	// that too. This is the raw write the Go-side monotonicity guard cannot see.
	database, err := bbolt.Open(cfg.MetadataPath(), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	zeroed := []byte(`{"generation":0,"sequence":0,"offset":0,"hash":` +
		`[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0]}`)
	if err := database.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte("meta")).Put([]byte("ledger_chain_checkpoint"), zeroed)
	}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	wiped, err := VerifyLedger(context.Background(), cfg)
	if err != nil {
		// The checkpoint still betrayed the wipe: better than the case below,
		// and the command exits non-zero either way.
		t.Logf("checkpoint refused the wipe: %v", err)
		return
	}
	t.Log("checkpoint accepted the wipe; only the caller's refusal separates this from a new install")
	if wiped.ChainVerified {
		t.Fatalf("a wiped ledger reported an authenticated chain: %#v", wiped)
	}
	if wiped.HoldsFrames() {
		t.Fatalf("expected the wipe to leave a report indistinguishable from a new install: %#v", wiped)
	}
	// Reaching here is the point: the report says "nothing to authenticate",
	// and only the caller's refusal to treat that as a pass separates this
	// directory from a fresh one.
}
