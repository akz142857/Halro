package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestDoctorDistinguishesLockContentionFromPermissions(t *testing.T) {
	permission := doctorDataLockFailure("/srv/halro/data", syscall.EACCES)
	if !strings.Contains(permission, "/srv/halro") || !strings.Contains(permission, "writable parent") {
		t.Fatalf("permission detail is not actionable: %q", permission)
	}
	contention := doctorDataLockFailure("/srv/halro/data", errors.Join(lock.ErrAlreadyLocked, syscall.EWOULDBLOCK))
	if !strings.Contains(contention, "another Halro process") || strings.Contains(contention, "writable parent") {
		t.Fatalf("contention detail is misleading: %q", contention)
	}
}

// The formatter above is only half the claim. This runs the real doctor
// against the state an operator actually reaches — a data_dir that no start
// ever wrote to, reached by following container instructions that failed — and
// asserts the report names that state instead of reporting a lock it could not
// acquire. Two different situations with two different next steps.
func TestDoctorReportsAnUninitializedDataDirAsSuch(t *testing.T) {
	cfg := testConfig(t)
	cfg.Storage.DataDir = filepath.Join(t.TempDir(), "never-initialized")
	if err := os.MkdirAll(cfg.Storage.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	report, err := DoctorWithOptions(context.Background(), cfg, DoctorOptions{NoKMS: true})
	if err == nil {
		t.Fatal("doctor passed on a data directory that was never initialized")
	}
	var detail string
	for _, check := range report.Checks {
		if check.Name == "data_lock" {
			detail = check.Detail
		}
	}
	if detail == "" {
		t.Fatalf("doctor produced no data_lock check: %#v", report.Checks)
	}
	if !strings.Contains(detail, "has not been initialized") || !strings.Contains(detail, "halro init") {
		t.Fatalf("data_lock detail does not name the state or the next step: %q", detail)
	}
	if strings.Contains(detail, "another Halro process") {
		t.Fatalf("an uninitialized directory was reported as lock contention: %q", detail)
	}
}

func TestKMSDoctorStaticAndFullModesAreReadOnly(t *testing.T) {
	cfg := kmsAppTestConfig(t)
	harness := newKMSAppHarness(t)
	if err := initializeKMS(context.Background(), cfg, kmsInitializationOptions{
		factory: harness.factory, random: bytes.NewReader(bytes.Repeat([]byte{0x55}, 32)),
	}); err != nil {
		t.Fatal(err)
	}
	previousFactory := defaultKMSWrapperFactory
	defaultKMSWrapperFactory = harness.factory
	t.Cleanup(func() { defaultKMSWrapperFactory = previousFactory })
	beforeTree := doctorTree(t, cfg.Storage.DataDir)
	beforeCalls := harness.callCount()
	staticReport, err := DoctorWithOptions(context.Background(), cfg, DoctorOptions{NoKMS: true})
	if err != nil {
		t.Fatal(err)
	}
	if staticReport.Healthy || staticReport.VaultStatus != "vault_unverified" || staticReport.ExternalAuditEvents {
		t.Fatalf("static report=%#v", staticReport)
	}
	if harness.callCount() != beforeCalls {
		t.Fatal("doctor --no-kms reached a KMS adapter")
	}
	if afterTree := doctorTree(t, cfg.Storage.DataDir); !reflect.DeepEqual(afterTree, beforeTree) {
		t.Fatal("static KMS doctor changed application files")
	}
	fullReport, err := Doctor(context.Background(), cfg)
	if err != nil || !fullReport.Healthy || fullReport.VaultStatus != "verified" || !fullReport.ExternalAuditEvents {
		t.Fatalf("full report=%#v err=%v", fullReport, err)
	}
	if harness.callCount() == beforeCalls {
		t.Fatal("full KMS doctor did not perform a read-only unwrap")
	}
	if afterTree := doctorTree(t, cfg.Storage.DataDir); !reflect.DeepEqual(afterTree, beforeTree) {
		t.Fatal("full KMS doctor changed application files")
	}
}

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
	before, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	user, err := before.store.GetAdminUser(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	user.Appearance = domain.AppearanceLight
	user.UpdatedAt = time.Now().UTC()
	if _, err := before.store.PutAdminUser(context.Background(), user, user.Revision); err != nil {
		t.Fatal(err)
	}
	if err := before.Close(); err != nil {
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
	resetUser, err := runtime.store.GetAdminUser(context.Background(), "admin")
	if err != nil || resetUser.Appearance != domain.AppearanceLight {
		t.Fatalf("reset password lost appearance: %#v err=%v", resetUser, err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if summary, err := VerifyAudit(context.Background(), cfg); err != nil || summary.Records < 2 {
		t.Fatalf("reset audit summary=%#v err=%v", summary, err)
	}
}
