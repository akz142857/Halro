package idempotency

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestDurableProjectScopedLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idempotency.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fp := Fingerprint("chat", []byte(`{"model":"chat"}`))
	record, created, err := store.Reserve(context.Background(), "prj_a", "retry-1", fp, "req_1", time.Hour)
	if err != nil || !created || record.KeyHash == "retry-1" {
		t.Fatalf("record=%+v created=%v err=%v", record, created, err)
	}
	if _, created, err := store.Reserve(context.Background(), "prj_a", "retry-1", fp, "req_2", time.Hour); err != nil || created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if _, _, err := store.Reserve(context.Background(), "prj_a", "retry-1", Fingerprint("chat", []byte(`{}`)), "req_3", time.Hour); !errors.Is(err, ErrConflict) {
		t.Fatalf("err=%v", err)
	}
	if _, created, err := store.Reserve(context.Background(), "prj_b", "retry-1", fp, "req_4", time.Hour); err != nil || !created {
		t.Fatalf("project scope failed: %v", err)
	}
	if _, err := store.Transition(context.Background(), "prj_a", "retry-1", InProgress); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), "prj_a", "retry-1", Completed); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.Get(context.Background(), "prj_a", "retry-1")
	if err != nil || got.State != Completed {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestUnknownIsTerminalAndKeysAreBounded(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "idempotency.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fp := Fingerprint("embeddings", []byte(`{"model":"embed"}`))
	if _, _, err := store.Reserve(context.Background(), "prj", "bad key", fp, "req", time.Hour); err == nil {
		t.Fatal("expected key rejection")
	}
	if _, _, err := store.Reserve(context.Background(), "prj", "retry", fp, "req", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), "prj", "retry", Unknown); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(context.Background(), "prj", "retry", Completed); !errors.Is(err, ErrTerminal) {
		t.Fatalf("err=%v", err)
	}
}
