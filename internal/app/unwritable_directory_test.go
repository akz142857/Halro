package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// An unwritable data directory has to stop the instance, not degrade it.
//
// The 2026-09-18 run recorded that a read-only filesystem had no automated
// coverage at all (260918-PV-F-21). This closes the half of it that can be
// closed without the target environment: a directory whose permissions deny
// writes. It is not the same errno a mounted read-only filesystem gives —
// EACCES rather than EROFS — but it exercises the same branch in every caller,
// which is the one that decides whether an instance that cannot write starts
// anyway.
//
// What remains for G4 in the target environment is the real mount: EROFS
// arriving *after* a descriptor is already open, which no permission change can
// reproduce because an open descriptor keeps its access.

// denyWrites makes a directory unwritable and restores it afterwards, or skips
// when the test cannot be meaningful.
func denyWrites(t *testing.T, directory string) {
	t.Helper()
	if os.Geteuid() == 0 {
		// root ignores the permission bits, so the write would succeed and the
		// test would assert the opposite of what it says.
		t.Skip("permission bits do not constrain root")
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(directory, info.Mode().Perm()) })
}

// TestInitializeRefusesAnUnwritableParent. Nothing has been created yet, so the
// only correct outcome is a refusal that names the directory.
func TestInitializeRefusesAnUnwritableParent(t *testing.T) {
	root := t.TempDir()
	cfg := crashConfig(root)
	denyWrites(t, root)

	if err := Initialize(cfg); err == nil {
		t.Fatal("initialization succeeded inside an unwritable directory")
	}
	// And it left nothing half-made behind.
	if _, err := os.Stat(cfg.Storage.DataDir); err == nil {
		t.Fatal("a refused initialization created the data directory anyway")
	}
}

// TestOpeningAnUnwritableDataDirectoryFailsClosed is the case that matters:
// the directory is a complete, valid instance, and the filesystem it sits on
// has stopped accepting writes.
//
// Starting anyway would be the worst outcome available — the Gateway would
// accept requests it cannot account for, and the Ledger is the accounting
// authority precisely because it is written before the upstream is called.
func TestOpeningAnUnwritableDataDirectoryFailsClosed(t *testing.T) {
	root := t.TempDir()
	cfg := crashConfig(root)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	denyWrites(t, cfg.Storage.DataDir)

	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		runtime.Close()
		t.Fatal("an instance opened on a data directory it cannot write to")
	}
}

// TestDoctorStillReadsAnUnwritableDataDirectory is the other half of the same
// property, and the reason the refusal above is not simply "refuse everything".
//
// doctor is the one command that writes nothing at all, so it has to keep
// working when the filesystem has gone read-only — that is exactly the moment
// an operator runs it. A diagnostic that needs write access to report that
// writes are failing would be useless in the situation it exists for.
func TestDoctorStillReadsAnUnwritableDataDirectory(t *testing.T) {
	root := t.TempDir()
	cfg := crashConfig(root)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	// The lock file has to stay writable for doctor's shared acquisition; the
	// directory itself is what goes read-only.
	denyWrites(t, cfg.Storage.DataDir)

	report, err := Doctor(context.Background(), cfg)
	if err != nil {
		t.Fatalf("doctor could not read an unwritable directory: %v", err)
	}
	if !report.Healthy {
		var failed []string
		for _, check := range report.Checks {
			if check.Status == "fail" {
				failed = append(failed, check.Name+": "+check.Detail)
			}
		}
		t.Fatalf("doctor reported an intact directory as unhealthy: %v", failed)
	}
}

// TestAnUnwritableLedgerDirectoryStillAppends records why permission bits
// cannot stand in for a read-only mount.
//
// This test was written expecting a refusal and got the opposite, which is the
// useful result. A directory's write bit governs creating and removing entries
// in it, not writing to a file that is already open — so an unwritable Ledger
// directory leaves appends working and only fails the operations that make new
// files: sealing a generation, staging a backup, publishing a snapshot.
//
// That is the precise reason 260918-PV-F-21's read-only case cannot be closed
// here. A real EROFS mount fails the append too, and nothing a test can do with
// chmod reproduces it. The assertion below pins the behaviour that does exist,
// so that if appends ever start failing on an unwritable directory, somebody
// finds out from a test rather than from an instance that will not start.
func TestAnUnwritableLedgerDirectoryStillAppends(t *testing.T) {
	root := t.TempDir()
	cfg := crashConfig(root)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	denyWrites(t, filepath.Dir(cfg.LedgerPath()))

	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("an existing Ledger file stopped accepting appends when its directory went read-only: %v", err)
	}
	defer runtime.Close()
	// And it really does append, rather than merely opening.
	if err := billOneCrashRequest(runtime, "req_unwritable_dir"); err != nil {
		t.Fatalf("accounting could not be written: %v", err)
	}
	// Creating a *new* entry in that directory is what fails, which is the half
	// of read-only behaviour permission bits can actually demonstrate.
	if err := os.WriteFile(filepath.Join(filepath.Dir(cfg.LedgerPath()), "probe"), nil, 0o600); err == nil {
		t.Fatal("a new file was created in a directory denied write permission")
	}
}
