package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/dirscan"
)

func TestRetainedCiphertextFixedTreeStopCancellationAndUnknown(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 97; i++ {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("empty-%03d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if present, err := retainedDirectoryHasObjectsContext(context.Background(), root); err != nil || present {
		t.Fatal(present, err)
	}
	if err := os.WriteFile(filepath.Join(root, "retained"), []byte("ciphertext"), 0600); err != nil {
		t.Fatal(err)
	}
	if present, err := retainedDirectoryHasObjectsContext(context.Background(), root); err != nil || !present {
		t.Fatal(present, err)
	}
	if err := os.Remove(filepath.Join(root, "retained")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(root, "unknown")); err != nil {
		t.Fatal(err)
	}
	if _, err := retainedDirectoryHasObjectsContext(context.Background(), root); !errors.Is(err, dirscan.ErrIdentity) {
		t.Fatal("unknown symlink licensed rotation", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := retainedDirectoryHasObjectsContext(ctx, "missing"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestTOTPWatermarkSizeCheckedBeforeReadAndEncode(t *testing.T) {
	cfg := testConfig(t)
	if err := os.MkdirAll(cfg.ClusterDirectoryPath(), 0700); err != nil {
		t.Fatal(err)
	}
	path := totpWatermarkPath(cfg)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(65 << 10); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if result, err := readTOTPWatermarks(cfg); !errors.Is(err, errBoundedFile) || result != nil {
		t.Fatal(result, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", path); err != nil {
		t.Fatal(err)
	}
	if _, err := readTOTPWatermarks(cfg); !errors.Is(err, errBoundedFile) {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := writeTOTPWatermarks(cfg, map[string]int64{"authenticator": 123}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tooMany := make(map[string]int64, 4097)
	for i := 0; i < 4097; i++ {
		tooMany[fmt.Sprintf("id-%04d", i)] = int64(i)
	}
	if err := writeTOTPWatermarks(cfg, tooMany); err == nil {
		t.Fatal("allocated entire oversized encoder output")
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
		t.Fatal("failed graph check changed watermark", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readTOTPWatermarksContext(ctx, cfg); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
