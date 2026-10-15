package app

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"go.etcd.io/bbolt"
)

// olderSchemaMetadata builds the one thing a diagnostic must refuse: a real
// metadata database written by an earlier release. It is produced by opening a
// current one and winding the recorded version back, which is what an operator
// who has not upgraded yet actually holds.
func olderSchemaMetadata(t *testing.T, version uint64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "halro.db")
	store, err := boltstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		encoded := make([]byte, 8)
		binary.BigEndian.PutUint64(encoded, version)
		return tx.Bucket([]byte("meta")).Put([]byte("schema_version"), encoded)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// The migration is one-way: once it has run, the previous release refuses the
// directory. A command whose whole purpose is to answer "is my data intact
// before I upgrade" must therefore not perform it as a side effect — which is
// what `halro ledger verify` did, while reporting a failure, so the operator
// read "do not upgrade" off a command that had already made upgrading
// irreversible.
func TestADiagnosticRefusesAnOlderSchemaInsteadOfMigratingIt(t *testing.T) {
	path := olderSchemaMetadata(t, boltstore.CurrentSchemaVersion()-2)

	err := assertMetadataSchemaCurrent(path)
	if !errors.Is(err, boltstore.ErrSchemaVersionMismatch) {
		t.Fatalf("err=%v, want boltstore.ErrSchemaVersionMismatch", err)
	}
	// The refusal has to name the way forward. "schema mismatch" on its own
	// reads as corruption to someone who has done nothing wrong.
	for _, want := range []string{"does not migrate", "halro start", "backup"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal does not mention %q: %v", want, err)
		}
	}

	// The point of the refusal: the directory is still readable by the release
	// that wrote it.
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var found uint64
	if err := db.View(func(tx *bbolt.Tx) error {
		found = binary.BigEndian.Uint64(tx.Bucket([]byte("meta")).Get([]byte("schema_version")))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := boltstore.CurrentSchemaVersion() - 2; found != want {
		t.Fatalf("the diagnostic migrated the directory to v%d; it must still be v%d", found, want)
	}
}

func TestADiagnosticAcceptsTheCurrentSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "halro.db")
	store, err := boltstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := assertMetadataSchemaCurrent(path); err != nil {
		t.Fatalf("a current data directory was refused: %v", err)
	}
}

// A missing database is not a schema question, and must not be reported as one:
// the operator's next move is different.
func TestADiagnosticReportsAMissingDatabaseAsItself(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")
	err := assertMetadataSchemaCurrent(path)
	if err == nil {
		t.Fatal("a missing metadata database was accepted")
	}
	if errors.Is(err, boltstore.ErrSchemaVersionMismatch) {
		t.Fatalf("a missing database was reported as a schema mismatch: %v", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("the diagnostic created a metadata database")
	}
}
