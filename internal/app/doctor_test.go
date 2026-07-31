package app

import (
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDoctorIsReadOnlyAndDetectsPartialWALTail(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	beforeTree := doctorTree(t, cfg.Storage.DataDir)
	report, err := Doctor(context.Background(), cfg)
	if err != nil || !report.Healthy {
		t.Fatalf("healthy doctor report=%#v err=%v", report, err)
	}
	if afterTree := doctorTree(t, cfg.Storage.DataDir); !reflect.DeepEqual(afterTree, beforeTree) {
		t.Fatalf("doctor changed application data\nbefore=%#v\nafter=%#v", beforeTree, afterTree)
	}
	file, err := os.OpenFile(cfg.LedgerPath(), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte{0x48, 0x4d, 0x44}); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(cfg.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	report, err = Doctor(context.Background(), cfg)
	if err == nil || report.Healthy {
		t.Fatalf("partial WAL passed doctor: %#v", report)
	}
	after, err := os.Stat(cfg.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("doctor repaired or mutated WAL: before=%d after=%d", before.Size(), after.Size())
	}
}

func doctorTree(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	result := make(map[string][32]byte)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result[relative] = sha256.Sum256(payload)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestResetAdminPasswordInvalidatesOldCredential(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	oldPassword := []byte("correct horse battery staple")
	newPassword := []byte("new correct horse battery staple")
	if err := BootstrapAdmin(context.Background(), cfg, "admin", oldPassword); err != nil {
		t.Fatal(err)
	}
	if err := ResetAdminPassword(context.Background(), cfg, "admin", newPassword); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	login := func(password string) int {
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, adminRequest(t, http.MethodPost,
			"/admin/api/v1/session/login", map[string]string{"username": "admin", "password": password}))
		return response.Code
	}
	if status := login(string(oldPassword)); status != http.StatusUnauthorized {
		t.Fatalf("old password status=%d", status)
	}
	if status := login(string(newPassword)); status != http.StatusOK {
		t.Fatalf("new password status=%d", status)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if summary, err := VerifyAudit(context.Background(), cfg); err != nil || summary.Records < 2 {
		t.Fatalf("reset audit summary=%#v err=%v", summary, err)
	}
}
