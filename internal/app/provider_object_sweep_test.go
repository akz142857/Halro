package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Two paths leave a sealed object nothing names: a submission that stores its
// object and then fails to store the record, and a settlement that writes the
// answer and then fails to save it. Nothing removed them — the startup
// reclamation walks records, so a file with no record is invisible to it, and
// the one scan that walks the directory skips sealed files by design. The
// material outlived every TTL the resource model promises, and doctor, backup
// and the reaper could none of them see it.
func TestAnOrphanedSealedObjectIsSweptOnceItCannotBeALiveWrite(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	// Real envelopes, because the startup reclamation that clears plaintext left
	// by releases before sealing runs on this same directory and removes
	// anything that is not one. That scan is why an orphan needs this sweep at
	// all: it walks the directory and skips every sealed file, deliberately,
	// since at that point it cannot tell a live write from an abandoned one.
	directory := filepath.Join(cfg.Storage.DataDir, "provider-objects")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	sealed, err := runtime.vault.EncryptResourceObject("resp_orphan:content", "project_1", []byte(`{"answer":"SECRET"}`))
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, age time.Duration) string {
		t.Helper()
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, sealed, 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().Add(-age)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		return path
	}
	orphan := write("resp_orphan.content", orphanSettlingPeriod+time.Hour)
	// What every legitimate write looks like in the window between its rename
	// and its record. The sweep must not touch it.
	fresh := write("resp_fresh.content", 0)

	runtime.sweepOrphanedProviderObjects(context.Background())

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("a sealed object no record names survived the sweep: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("the sweep raced a write that could still have been in flight: %v", err)
	}
}

// The sweep decides by record as well as by age: an object a record still names
// belongs to that record's TTL, however old the file is. Stated against the
// decision rather than against a data directory, because building a record the
// store accepts means building a whole topology to hold it.
func TestTheSweepKeepsWhatARecordStillNames(t *testing.T) {
	directory := t.TempDir()
	write := func(name string, age time.Duration) {
		t.Helper()
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte("sealed"), 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().Add(-age)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	write("named.content", 90*24*time.Hour)
	write("orphan.content", 90*24*time.Hour)
	write("fresh.content", time.Minute)
	write(".resource-halfwritten", 90*24*time.Hour)

	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	named := map[string]struct{}{"named.content": {}}
	swept := sweepableObjects(entries, named, time.Now().UTC().Add(-orphanSettlingPeriod))

	got := map[string]bool{}
	for _, name := range swept {
		got[name] = true
	}
	if got["named.content"] {
		t.Fatal("an object a record still names was swept")
	}
	if got["fresh.content"] {
		t.Fatal("the sweep raced a write that could still have been in flight")
	}
	if !got["orphan.content"] {
		t.Fatal("an aged object no record names survived")
	}
	if !got[".resource-halfwritten"] {
		t.Fatal("a temporary name left by a dead write survived")
	}
}
