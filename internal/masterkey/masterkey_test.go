package masterkey

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/vault"
)

func TestFileStoreLifecycle(t *testing.T) {
	store, err := NewFileStore(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	exists, err := store.Exists(context.Background())
	if err != nil || !exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
	key, err := store.Unlock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	if len(key) != vault.MasterKeySize {
		t.Fatalf("key length=%d", len(key))
	}
}

func TestNewUnlockerRejectsUnavailableKeySlotsWithoutIO(t *testing.T) {
	keySlots := config.MasterKey{Mode: config.MasterKeyModeKeySlots}
	for name, operation := range map[string]func() error{
		"unlocker":   func() error { _, err := NewUnlocker(keySlots); return err },
		"file store": func() error { _, err := FileStoreFromConfig(keySlots); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := operation(); !errors.Is(err, ErrModeUnavailable) {
				t.Fatalf("expected unavailable mode error, got %v", err)
			}
		})
	}
}

func TestFileStoreHonorsCancelledContext(t *testing.T) {
	store, err := NewFileStore(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Initialize(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}

func TestFileStoreReplacementRequiresOriginalUnlockedIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	next := make([]byte, vault.MasterKeySize)
	next[0] = 22
	if err := store.Replace(context.Background(), next); err == nil {
		t.Fatal("replacement accepted without original unlock")
	}
	original, err := store.Unlock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer clear(original)
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	foreign := make([]byte, vault.MasterKeySize)
	foreign[0] = 33
	if err := os.WriteFile(path, foreign, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Replace(context.Background(), next); err == nil {
		t.Fatal("late foreign live key overwritten")
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, foreign) {
		t.Fatal("foreign key changed", err)
	}
}
