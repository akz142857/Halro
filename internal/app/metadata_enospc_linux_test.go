//go:build linux

package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"golang.org/x/sys/unix"
)

func TestRuntimeMetadataActualLinuxENOSPCRetiresTrafficAndRecovers(t *testing.T) {
	root := os.Getenv("HALRO_RUNTIME_ENOSPC_ROOT")
	if root == "" {
		t.Skip("requires explicit dedicated small Linux tmpfs")
	}
	named, err := os.Lstat(root)
	if !filepath.IsAbs(root) || err != nil || !named.IsDir() || named.Mode()&os.ModeSymlink != 0 {
		t.Fatal("ENOSPC root must be an absolute real directory", err)
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type != 0x01021994 || uint64(fs.Blocks)*uint64(fs.Bsize) > 32<<20 {
		t.Fatal("refusing to fill a non-tmpfs or a mount over 32 MiB")
	}
	directory, err := os.MkdirTemp(root, "halro-runtime-metadata-enospc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	cfg := testConfig(t)
	cfg.Storage.DataDir = filepath.Join(directory, "data")
	cfg.Storage.MasterKey.File = filepath.Join(directory, "master.key")
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.stopBackgroundWork()
	native, err := os.OpenFile(r.store.MetadataJournalPath(), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Fallocate(int(native.Fd()), unix.FALLOC_FL_KEEP_SIZE, 0, 256<<10); err != nil {
		_ = native.Close()
		t.Fatal(err)
	}
	if err := errors.Join(native.Sync(), native.Close()); err != nil {
		t.Fatal(err)
	}
	fillerPath := filepath.Join(directory, "fill.tmpfs")
	filler, err := os.OpenFile(fillerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 64<<10)
	var filled int64
	for {
		n, writeErr := filler.Write(chunk)
		filled += int64(n)
		if writeErr != nil {
			if !errors.Is(writeErr, syscall.ENOSPC) {
				_ = filler.Close()
				t.Fatal(writeErr)
			}
			break
		}
		if filled > 32<<20 {
			_ = filler.Close()
			t.Fatal("dedicated volume exceeded guarded limit")
		}
	}
	if err := errors.Join(filler.Sync(), filler.Close()); err != nil {
		t.Fatal(err)
	}
	if err := unix.Statfs(root, &fs); err != nil || fs.Bavail != 0 {
		t.Fatal("fixture did not exhaust physical blocks", fs.Bavail, err)
	}
	project := domain.Project{ID: "durable-project", Name: "durable project", Enabled: true, AllowedModels: []string{strings.Repeat("m", 48<<10)}}
	_, commitErr := r.store.PutProject(ctx, project, 0, nil)
	if !errors.Is(commitErr, syscall.ENOSPC) || !errors.Is(commitErr, boltstore.ErrMetadataRecoveryRequired) {
		t.Fatal("projection commit did not retire on actual ENOSPC", commitErr)
	}
	for _, path := range []string{"/health/ready", "/v1/models", "/v1/chat/completions", "/v1/messages"} {
		method := http.MethodGet
		if strings.Contains(path, "completions") || strings.Contains(path, "messages") {
			method = http.MethodPost
		}
		response := httptest.NewRecorder()
		r.gatewayHandler().ServeHTTP(response, httptest.NewRequest(method, path, strings.NewReader(`{}`)))
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "recovery_required") {
			t.Fatal("metadata failure served traffic or stale authorization", path, response.Code, response.Body.String())
		}
	}
	live := httptest.NewRecorder()
	r.gatewayHandler().ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if live.Code != http.StatusOK {
		t.Fatal("recovery requirement incorrectly killed process liveness", live.Code)
	}
	if err := os.Remove(fillerPath); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); !errors.Is(err, boltstore.ErrMetadataRecoveryRequired) {
		t.Fatal("Close must report the uncommitted final checkpoint while joining all handles", err)
	}
	next, err := Open(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	actual, err := next.store.GetProject(ctx, project.ID)
	if err != nil || len(actual.AllowedModels) != 1 || actual.AllowedModels[0] != project.AllowedModels[0] {
		t.Fatal("Runtime reopen lost the native durable fact", err)
	}
	ready := httptest.NewRecorder()
	next.gatewayHandler().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if ready.Code != http.StatusOK {
		t.Fatal("authenticated recovery did not restore readiness", ready.Code, ready.Body.String())
	}
	t.Logf("actual tmpfs ENOSPC: filled=%d, free_blocks_at_failure=%d, commit_error=%v; Runtime refused stale traffic and reopened from native fact", filled, fs.Bavail, commitErr)
}
