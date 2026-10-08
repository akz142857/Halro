package usage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestArchiveBackupWalkRequiresCompletePolicyAndPreservesFixedSlots(t *testing.T) {
	f, e := dateActivationFixture(t, FormatNDJSON)
	ctx := context.Background()
	compareSuccession(t, f, e, archiveDateCutoff, false)
	if _, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff); err != nil {
		t.Fatal(err)
	}
	if err := e.WalkArchiveBackupFiles(ctx, f.a.Watermark(), func(string) error { return nil }); err == nil {
		t.Fatal("incomplete original obligation silently dropped from backup")
	}
	completeSuccession(t, f, e, archiveDateCutoff)
	seen := map[string]bool{}
	if err := e.WalkArchiveBackupFiles(ctx, f.a.Watermark(), func(path string) error {
		if seen[path] {
			t.Fatal("duplicate backup reference", path)
		}
		seen[path] = true
		if _, err := os.Lstat(filepath.Join(e.root, filepath.FromSlash(path))); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manifest.json", archiveDateActivationName, archiveDateActivePolicyName, archiveDatePagesName} {
		if !seen[name] {
			t.Fatal("backup omitted fixed policy/control", name)
		}
	}
	closedActive := false
	if err := e.walkArchiveBackupFiles(ctx, f.a.Watermark(), func(string) error { return nil }, func(file *os.File) error {
		if filepath.Base(file.Name()) == archiveDateActivePolicyName {
			closedActive = true
			return file.Close()
		}
		return nil
	}); err == nil || !closedActive {
		t.Fatal("actual active-control Close failure escaped backup walk", err, closedActive)
	}
	if err := os.WriteFile(filepath.Join(e.root, archiveDatePagesName), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.WalkArchiveBackupFiles(ctx, f.a.Watermark(), func(string) error { return nil }); err == nil {
		t.Fatal("backup accepted damaged irreversible progress")
	}
}

func TestArchiveBackupWalkPhysicalCallbackJoinsClose(t *testing.T) {
	f, e, _ := windowBindingFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	walk := make(chan error, 1)
	go func() {
		walk <- e.WalkArchiveBackupFiles(context.Background(), f.a.Watermark(), func(string) error { close(entered); <-release; return context.Canceled })
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- e.Close() }()
	deadline := time.Now().Add(3 * time.Second)
	for !e.archiveRetired.Load() {
		if time.Now().After(deadline) {
			t.Fatal("Close did not withdraw archive")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-closed:
		t.Fatal("Close passed physical backup callback", err)
	default:
	}
	unblock()
	if err := <-walk; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
