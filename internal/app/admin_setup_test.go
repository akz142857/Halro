package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/audit"
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
	cfg.TLS.CertFile = "/tmp/cert.pem"
	cfg.TLS.KeyFile = "/tmp/key.pem"
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	token, required, err := runtime.SetupToken(context.Background())
	if err != nil || !required || token == "" {
		t.Fatalf("token=%q required=%v err=%v", token, required, err)
	}
	// Login spray must not lock an operator holding the high-entropy one-time
	// token out of completing a public-listener setup.
	for i := 0; i < cfg.Admin.LoginRPM; i++ {
		if !runtime.allowAdminLogin("192.0.2.10:1234", time.Now()) {
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
	if throttledResponse.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled token status=%d body=%s", throttledResponse.Code, throttledResponse.Body.String())
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
	if token, required, err := runtime.SetupToken(context.Background()); err != nil || required || token != "" {
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
