package audit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadOnlyInspectionCeilingCancellationReplacementAndPartialTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.wal")
	key := randomKey(t)
	log, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := log.Append(context.Background(), validEvent(i+1, "inspect.test")); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if report, err := VerifyWithVisitorContext(context.Background(), path, key, int64(len(raw)-1), nil); !errors.Is(err, ErrJournalDiskLimit) || report != (Summary{}) {
		t.Fatal(report, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if report, err := VerifyWithVisitorContext(ctx, path, key, int64(len(raw)), func(Record) error { cancel(); return nil }); !errors.Is(err, context.Canceled) || report != (Summary{}) {
		t.Fatal(report, err)
	}
	replaced := false
	if report, err := VerifyWithVisitorContext(context.Background(), path, key, int64(len(raw)), func(Record) error {
		if replaced {
			return nil
		}
		replaced = true
		if err := os.Rename(path, path+".original"); err != nil {
			return err
		}
		return os.WriteFile(path, raw, 0600)
	}); !errors.Is(err, ErrCorrupt) || report != (Summary{}) {
		t.Fatal(report, err)
	}
	if err := os.WriteFile(path, append(raw, 0x05), 0600); err != nil {
		t.Fatal(err)
	}
	if report, err := VerifyWithVisitorContext(context.Background(), path, key, int64(len(raw)+1), nil); !errors.Is(err, ErrCorrupt) || report != (Summary{}) {
		t.Fatal(report, err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, append(raw, 0x05)) {
		t.Fatal("read-only verification repaired tail", err)
	}
}
