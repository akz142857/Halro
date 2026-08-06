package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The console cannot stop offering what the server will refuse unless it knows
// the role, and it only ever learns that from a session payload. A path that
// issues a session without one leaves the console guessing — so every such path
// is checked here rather than only the one login route.
func TestEverySessionPayloadCarriesTheRole(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	adminPassword := "correct horse battery staple"
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte(adminPassword)); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	roleOf := func(t *testing.T, body []byte) string {
		t.Helper()
		var payload struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("payload is not JSON: %v (%s)", err, body)
		}
		return payload.Role
	}

	// Login.
	login := adminRequest(t, http.MethodPost, "/admin/api/v1/session/login", map[string]string{
		"username": "admin", "password": adminPassword,
	})
	loginResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(loginResponse, login)
	if loginResponse.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", loginResponse.Code, loginResponse.Body.String())
	}
	if role := roleOf(t, loginResponse.Body.Bytes()); role != "administrator" {
		t.Fatalf("login role=%q, want administrator", role)
	}
	session := loginTestAdmin(t, runtime, "admin", adminPassword)

	// Session read-back.
	current := adminRequest(t, http.MethodGet, "/admin/api/v1/session", nil)
	current.AddCookie(session.cookie)
	currentResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(currentResponse, current)
	if currentResponse.Code != http.StatusOK {
		t.Fatalf("session status=%d body=%s", currentResponse.Code, currentResponse.Body.String())
	}
	if role := roleOf(t, currentResponse.Body.Bytes()); role != "administrator" {
		t.Fatalf("session role=%q, want administrator", role)
	}

	// A read-only account reports its own role, not the role of whoever made it.
	create := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/admin-users", session, map[string]string{
		"username": "viewer", "password": "another correct horse battery staple", "role": "read_only",
		"current_password": adminPassword,
	})
	createResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createResponse.Code, createResponse.Body.String())
	}
	viewerLogin := adminRequest(t, http.MethodPost, "/admin/api/v1/session/login", map[string]string{
		"username": "viewer", "password": "another correct horse battery staple",
	})
	viewerResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(viewerResponse, viewerLogin)
	if viewerResponse.Code != http.StatusOK {
		t.Fatalf("viewer login status=%d body=%s", viewerResponse.Code, viewerResponse.Body.String())
	}
	if role := roleOf(t, viewerResponse.Body.Bytes()); role != "read_only" {
		t.Fatalf("viewer login role=%q, want read_only", role)
	}
}

// First run issues a session too, and it is the one session a new operator sees
// before anything else exists to tell them what they are allowed to do.
func TestFirstRunSetupPayloadCarriesTheRole(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	runtime.setupMu.Lock()
	token := runtime.setupToken
	runtime.setupMu.Unlock()

	setup := adminRequest(t, http.MethodPost, "/admin/api/v1/setup/admin", map[string]string{
		"username": "admin", "password": "correct horse battery staple",
		"password_confirmation": "correct horse battery staple", "setup_token": token,
	})
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, setup)
	if response.Code != http.StatusCreated {
		t.Fatalf("setup status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Role != "administrator" {
		t.Fatalf("setup role=%q, want administrator — the first account owns the instance", payload.Role)
	}
}
