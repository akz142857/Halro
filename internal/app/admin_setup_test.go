package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	halroconfig "github.com/akz142857/Halro/internal/config"
)

func TestAdminSetupCreatesFirstUserAndSessionOnce(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	statusResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(statusResponse, adminRequest(
		t, http.MethodGet, "/admin/api/v1/setup/status", nil,
	))
	if statusResponse.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", statusResponse.Code, statusResponse.Body.String())
	}
	var status SetupStatus
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.InstanceInitialized || !status.SetupRequired || status.TokenRequired {
		t.Fatalf("unexpected setup status: %#v", status)
	}

	setup := adminRequest(t, http.MethodPost, "/admin/api/v1/setup/admin", map[string]string{
		"username": " admin ", "password": "correct horse battery staple",
		"password_confirmation": "correct horse battery staple",
	})
	setupResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(setupResponse, setup)
	if setupResponse.Code != http.StatusCreated {
		t.Fatalf("setup status=%d body=%s", setupResponse.Code, setupResponse.Body.String())
	}
	if !jsonBodyContains(t, setupResponse, `"appearance":"dark"`) {
		t.Fatalf("setup response did not expose default appearance: %s", setupResponse.Body.String())
	}
	cookies := setupResponse.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != adminSessionCookie {
		t.Fatalf("setup did not create a session: %#v", cookies)
	}
	sessionRequest := adminRequest(t, http.MethodGet, "/admin/api/v1/session", nil)
	sessionRequest.AddCookie(cookies[0])
	sessionResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(sessionResponse, sessionRequest)
	if sessionResponse.Code != http.StatusOK {
		t.Fatalf("created session status=%d body=%s", sessionResponse.Code, sessionResponse.Body.String())
	}
	if !jsonBodyContains(t, sessionResponse, `"appearance":"dark"`) {
		t.Fatalf("created session did not expose default appearance: %s", sessionResponse.Body.String())
	}
	foundBootstrapAudit := false
	if _, err := runtime.audit.Replay(func(record audit.Record) error {
		if record.Event.Action == "admin.bootstrap" && record.Event.ActorID == "admin" {
			foundBootstrapAudit = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !foundBootstrapAudit {
		t.Fatal("setup did not append the admin.bootstrap audit event")
	}

	repeatResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(repeatResponse, adminRequest(
		t, http.MethodPost, "/admin/api/v1/setup/admin", map[string]string{
			"username": "other", "password": "another strong password",
			"password_confirmation": "another strong password",
		},
	))
	if repeatResponse.Code != http.StatusConflict {
		t.Fatalf("repeat status=%d body=%s", repeatResponse.Code, repeatResponse.Body.String())
	}
	count, err := runtime.store.AdminUserCount(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("admin count=%d err=%v", count, err)
	}
}

func TestAdminSetupRequiresTransientTokenForPublicAdmin(t *testing.T) {
	cfg := testConfig(t)
	cfg.Server.AdminListen = "0.0.0.0:18081"
	cfg.TLS.Enabled = true
	cfg.TLS.Certificates = []halroconfig.TLSCertificate{{CertFile: "/tmp/cert.pem", KeyFile: "/tmp/key.pem"}}
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	token, required, err := runtime.TakeGeneratedSetupToken(context.Background())
	if err != nil || !required || token == "" {
		t.Fatalf("token=%q required=%v err=%v", token, required, err)
	}
	// Login spray must not lock an operator holding the high-entropy one-time
	// token out of completing a public-listener setup.
	for i := 0; i < cfg.Admin.LoginRPM; i++ {
		if allowed, _ := runtime.allowAdminLogin("192.0.2.10:1234", time.Now()); !allowed {
			t.Fatal("failed to populate login rate limit")
		}
	}
	throttled := adminRequest(t, http.MethodPost, "/admin/api/v1/setup/admin", map[string]string{
		"username": "admin", "password": "correct horse battery staple",
		"password_confirmation": "correct horse battery staple", "setup_token": "wrong-token",
	})
	throttled.RemoteAddr = "192.0.2.10:1234"
	throttledResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(throttledResponse, throttled)
	if throttledResponse.Code != http.StatusForbidden {
		t.Fatalf("setup limiter shared login state: status=%d body=%s", throttledResponse.Code, throttledResponse.Body.String())
	}

	for _, candidate := range []string{"", "wrong-token", token} {
		response := httptest.NewRecorder()
		request := adminRequest(
			t, http.MethodPost, "/admin/api/v1/setup/admin", map[string]string{
				"username": "admin", "password": "correct horse battery staple",
				"password_confirmation": "correct horse battery staple", "setup_token": candidate,
			},
		)
		if candidate == token {
			request.RemoteAddr = "192.0.2.10:1234"
		} else {
			request.RemoteAddr = "198.51.100.10:1234"
		}
		runtime.adminRouter().ServeHTTP(response, request)
		if candidate == token && response.Code != http.StatusCreated {
			t.Fatalf("valid token status=%d body=%s", response.Code, response.Body.String())
		}
		if candidate != token && response.Code != http.StatusForbidden {
			t.Fatalf("invalid token status=%d body=%s", response.Code, response.Body.String())
		}
	}
	if token, required, err := runtime.TakeGeneratedSetupToken(context.Background()); err != nil || required || token != "" {
		t.Fatalf("token survived setup: %q required=%v err=%v", token, required, err)
	}
}

func TestConcurrentAdminSetupCreatesExactlyOneUser(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	var wait sync.WaitGroup
	statuses := make(chan int, 2)
	for _, username := range []string{"first", "second"} {
		wait.Add(1)
		go func(username string) {
			defer wait.Done()
			request := adminRequest(t, http.MethodPost, "/admin/api/v1/setup/admin", map[string]string{
				"username": username, "password": "correct horse battery staple",
				"password_confirmation": "correct horse battery staple",
			})
			response := httptest.NewRecorder()
			runtime.adminRouter().ServeHTTP(response, request)
			statuses <- response.Code
		}(username)
	}
	wait.Wait()
	close(statuses)
	created, conflicts := 0, 0
	for status := range statuses {
		if status == http.StatusCreated {
			created++
		}
		if status == http.StatusConflict {
			conflicts++
		}
	}
	if created != 1 || conflicts != 1 {
		t.Fatalf("created=%d conflicts=%d", created, conflicts)
	}
}

func TestAdminSetupRejectsDNSRebindingHost(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	request := adminRequest(t, http.MethodPost, "/admin/api/v1/setup/admin", map[string]string{
		"username": "attacker", "password": "correct horse battery staple",
		"password_confirmation": "correct horse battery staple",
	})
	request.Host = "attacker.example"
	request.Header.Set("Origin", "http://attacker.example")
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	count, err := runtime.store.AdminUserCount(context.Background())
	if err != nil || count != 0 {
		t.Fatalf("admin count=%d err=%v", count, err)
	}
}

func TestAdminSetupUsesFileTokenWithoutExposingIt(t *testing.T) {
	cfg := testConfig(t)
	path := filepath.Join(t.TempDir(), "setup-token")
	if err := GenerateSetupTokenFile(path); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.SplitN(string(payload), "\n", 2)[0]
	cfg.Admin.SetupTokenFile = path
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	runtime, err := OpenWithOptions(context.Background(), cfg, slog.New(slog.NewTextHandler(&logs, nil)), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if exposed, available, err := runtime.TakeGeneratedSetupToken(context.Background()); err != nil || available || exposed != "" {
		t.Fatalf("file token exposed=%q available=%v err=%v", exposed, available, err)
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), path) {
		t.Fatalf("setup-token log disclosed secret material: %s", logs.String())
	}
	if runtime.verifySetupToken(strings.Repeat("x", setupTokenInputLimit+1)) {
		t.Fatal("oversized setup token was accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if !runtime.verifySetupToken(token) {
		t.Fatal("running process did not retain the token it loaded before Secret deletion")
	}

	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, adminRequest(t, http.MethodPost, "/admin/api/v1/setup/admin", map[string]string{
		"username": "admin", "password": "correct horse battery staple",
		"password_confirmation": "correct horse battery staple", "setup_token": token,
	}))
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if runtime.verifySetupToken(token) {
		t.Fatal("setup token remained valid after the administrator commit")
	}
}

type cancelOnHeaderWriter struct {
	header http.Header
	cancel context.CancelFunc
	once   sync.Once
	status int
}

func (writer *cancelOnHeaderWriter) Header() http.Header {
	writer.once.Do(writer.cancel)
	return writer.header
}

func (writer *cancelOnHeaderWriter) Write(payload []byte) (int, error) {
	return len(payload), nil
}

func (writer *cancelOnHeaderWriter) WriteHeader(status int) {
	writer.status = status
}

func TestAdminSetupCommitClosesSetupEvenWhenSessionCreationFails(t *testing.T) {
	cfg := testConfig(t)
	path := filepath.Join(t.TempDir(), "setup-token")
	if err := GenerateSetupTokenFile(path); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.SplitN(string(payload), "\n", 2)[0]
	cfg.Admin.SetupTokenFile = path
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := OpenWithOptions(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	ctx, cancel := context.WithCancel(context.Background())
	request := adminRequest(t, http.MethodPost, "/admin/api/v1/setup/admin", map[string]string{
		"username": "admin", "password": "correct horse battery staple",
		"password_confirmation": "correct horse battery staple", "setup_token": token,
	}).WithContext(ctx)
	writer := &cancelOnHeaderWriter{header: make(http.Header), cancel: cancel}
	runtime.setupAdmin(writer, request)
	if writer.status != http.StatusConflict {
		t.Fatalf("status=%d, want committed-login response", writer.status)
	}
	count, err := runtime.store.AdminUserCount(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("admin count=%d err=%v", count, err)
	}
	if runtime.verifySetupToken(token) {
		t.Fatal("setup reopened after post-commit session failure")
	}
}

func TestAdminSetupTokenExpires(t *testing.T) {
	cfg := testConfig(t)
	path := filepath.Join(t.TempDir(), "setup-token")
	if err := GenerateSetupTokenFile(path); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.SplitN(string(payload), "\n", 2)[0]
	cfg.Admin.SetupTokenFile = path
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := OpenWithOptions(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	runtime.setupMu.Lock()
	expiresAt := runtime.setupToken.expiresAt
	runtime.setupMu.Unlock()
	runtime.now = func() time.Time { return expiresAt }
	if runtime.verifySetupToken(token) {
		t.Fatal("expired setup token was accepted")
	}
}

func TestServeRequiresAProvisionedTokenForRemoteFirstSetup(t *testing.T) {
	cfg := testConfig(t)
	cfg.Server.AdminListen = "0.0.0.0:18081"
	cfg.TLS.Enabled = true
	cfg.TLS.Certificates = []halroconfig.TLSCertificate{{CertFile: "/tmp/cert.pem", KeyFile: "/tmp/key.pem"}}
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	_, err := OpenWithOptions(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{})
	if err == nil || !strings.Contains(err.Error(), "admin.setup_token_file") {
		t.Fatalf("error=%v", err)
	}
}

func TestConfiguredSetupTokenFileMustBeReadableForFirstSetup(t *testing.T) {
	cfg := testConfig(t)
	path := filepath.Join(t.TempDir(), "missing-setup-token")
	cfg.Admin.SetupTokenFile = path
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	_, err := OpenWithOptions(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{})
	if err == nil || !strings.Contains(err.Error(), "file is not readable") {
		t.Fatalf("error=%v", err)
	}
	if strings.Contains(err.Error(), path) {
		t.Fatalf("error disclosed configured secret path: %v", err)
	}
}

func TestInitializedInstanceDoesNotReadMissingSetupTokenFile(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	cfg.Admin.SetupTokenFile = filepath.Join(t.TempDir(), "deleted-token")
	runtime, err := OpenWithOptions(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{})
	if err != nil {
		t.Fatalf("initialized instance depended on deleted token file: %v", err)
	}
	defer runtime.Close()
}
