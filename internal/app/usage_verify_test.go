package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `halro usage verify` is one of the first commands an operator runs against a
// new data directory, and a directory that has been initialized but has never
// exported has no Parquet manifest yet. It still fails — an erased usage tree
// presents the same way — but it used to fail with a bare "no such file or
// directory", which reads as data loss on an install that has none.
func TestVerifyUsageSaysWhichMissingManifestStateItFound(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	_, err := VerifyUsage(context.Background(), cfg)
	if err == nil {
		t.Fatal("verify certified a directory with no usage manifest")
	}
	for _, expected := range []string{"holds no manifest", "has not exported usage yet", "erased"} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("verify error %q does not contain %q", err, expected)
		}
	}
	if strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("verify still answers with a bare filesystem error: %v", err)
	}
}

// The manifest is there and lists a partition whose file is gone, while the
// ledger holds nothing — a directory whose accounting authority was wiped or
// restored empty under a usage tree that survived. Verify hashes every listed
// partition, so the error it raises is the same *os.PathError a missing
// manifest produces: inferring "nothing exported yet" from the error type alone
// certifies exactly this state as clean.
func TestVerifyUsageFailsWhenAnEmptyLedgerSitsUnderAManifestMissingItsPartition(t *testing.T) {
	source := testConfig(t)
	if err := Initialize(source); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), source, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	billOneRequest(t, runtime, "verify_partition_loss")
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	manifest, err := CompactUsage(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) == 0 {
		t.Fatal("compaction produced no partition to remove")
	}

	// A fresh directory: empty ledger, empty aggregate, no manifest of its own.
	target := testConfig(t)
	if err := Initialize(target); err != nil {
		t.Fatal(err)
	}
	copyUsageTree(t, source.UsagePath(), target.UsagePath())
	if err := os.Remove(filepath.Join(target.UsagePath(), manifest.Files[0].Path)); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyUsage(context.Background(), target); err == nil {
		t.Fatal("verify certified a manifest whose partition file is gone")
	}
}

func copyUsageTree(t *testing.T, source, target string) {
	t.Helper()
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		from := filepath.Join(source, entry.Name())
		to := filepath.Join(target, entry.Name())
		if entry.IsDir() {
			if err := os.MkdirAll(to, 0o700); err != nil {
				t.Fatal(err)
			}
			copyUsageTree(t, from, to)
			continue
		}
		payload, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// The other half: once the ledger holds billed work, a manifest that is gone is
// real divergence and must stay an error. Without this, the carve-out above
// would turn deleted usage history into a clean bill of health.
func TestVerifyUsageStillFailsWhenABilledDirectoryLostItsManifest(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	billOneRequest(t, runtime, "verify_manifest_loss")
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := CompactUsage(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(cfg.UsagePath(), "manifest.json")
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyUsage(context.Background(), cfg); err == nil {
		t.Fatal("verify certified a billed directory whose usage manifest is gone")
	}
}
