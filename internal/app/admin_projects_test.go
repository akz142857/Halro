package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/auth"
	"github.com/akz142857/Heimdall/internal/domain"
)

func TestAdminProjectAndKeyLifecycle(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(
		context.Background(), cfg, "admin", []byte("correct horse battery staple"),
	); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, csrf := loginAdminForTest(t, runtime)

	projectBody := map[string]any{
		"name": "Inference", "enabled": true, "allowed_routes": []string{"chat"},
		"rpm": int64(60), "tpm": int64(100_000), "max_concurrency": int64(8),
		"daily_budget_micros_usd":     int64(5_000_000),
		"max_input_tokens":            int64(32_000),
		"max_output_tokens":           int64(4_096),
		"max_request_bytes":           int64(1 << 20),
		"max_stream_duration_seconds": int64(120),
		"allowed_cidrs":               []string{"10.0.0.9/24"},
	}
	rejected := adminRequest(t, http.MethodPost, "/admin/api/v1/projects", projectBody)
	rejected.AddCookie(cookie)
	rejectedResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(rejectedResponse, rejected)
	if rejectedResponse.Code != http.StatusForbidden {
		t.Fatalf("project create without CSRF status=%d", rejectedResponse.Code)
	}

	create := adminRequest(t, http.MethodPost, "/admin/api/v1/projects", projectBody)
	create.AddCookie(cookie)
	create.Header.Set("X-CSRF-Token", csrf)
	createResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusCreated || createResponse.Header().Get("ETag") != `"1"` {
		t.Fatalf("project create status=%d body=%s", createResponse.Code, createResponse.Body.String())
	}
	var project domain.Project
	if err := json.Unmarshal(createResponse.Body.Bytes(), &project); err != nil {
		t.Fatal(err)
	}
	if len(project.AllowedCIDRs) != 1 || project.AllowedCIDRs[0].String() != "10.0.0.0/24" {
		t.Fatalf("CIDR was not normalized: %#v", project.AllowedCIDRs)
	}

	createKey := adminRequest(t, http.MethodPost,
		"/admin/api/v1/projects/"+project.ID+"/keys",
		map[string]any{"name": "service-a"},
	)
	createKey.AddCookie(cookie)
	createKey.Header.Set("X-CSRF-Token", csrf)
	createKeyResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(createKeyResponse, createKey)
	if createKeyResponse.Code != http.StatusCreated ||
		createKeyResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("key create status=%d body=%s", createKeyResponse.Code, createKeyResponse.Body.String())
	}
	var keyResult struct {
		Key      string         `json:"key"`
		Metadata gatewayKeyView `json:"metadata"`
	}
	if err := json.Unmarshal(createKeyResponse.Body.Bytes(), &keyResult); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(keyResult.Key, auth.GatewayKeyPrefix) ||
		strings.Contains(createKeyResponse.Body.String(), "key_hash") {
		t.Fatalf("invalid key creation response: %s", createKeyResponse.Body.String())
	}
	if _, err := runtime.auth.Authenticate(keyResult.Key, time.Now()); err != nil {
		t.Fatalf("new key is not active in auth snapshot: %v", err)
	}
	unblock := adminRequest(t, http.MethodPost, "/admin/api/v1/projects/"+project.ID+"/unblock", nil)
	unblock.AddCookie(cookie)
	unblock.Header.Set("X-CSRF-Token", csrf)
	unblockResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(unblockResponse, unblock)
	if unblockResponse.Code != http.StatusOK || !strings.Contains(unblockResponse.Body.String(), `"status":"unblocked"`) {
		t.Fatalf("project unblock status=%d body=%s", unblockResponse.Code, unblockResponse.Body.String())
	}

	getKey := adminRequest(t, http.MethodGet,
		"/admin/api/v1/projects/"+project.ID+"/keys/"+keyResult.Metadata.ID, nil,
	)
	getKey.AddCookie(cookie)
	getKeyResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(getKeyResponse, getKey)
	if getKeyResponse.Code != http.StatusOK ||
		strings.Contains(getKeyResponse.Body.String(), keyResult.Key) ||
		strings.Contains(getKeyResponse.Body.String(), "key_hash") {
		t.Fatalf("key read leaked secret status=%d body=%s", getKeyResponse.Code, getKeyResponse.Body.String())
	}

	disable := adminRequest(t, http.MethodPut,
		"/admin/api/v1/projects/"+project.ID+"/keys/"+keyResult.Metadata.ID,
		map[string]any{"name": "service-a", "enabled": false},
	)
	disable.AddCookie(cookie)
	disable.Header.Set("X-CSRF-Token", csrf)
	disable.Header.Set("If-Match", `"99"`)
	staleResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(staleResponse, disable)
	if staleResponse.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale update status=%d body=%s", staleResponse.Code, staleResponse.Body.String())
	}

	disable = adminRequest(t, http.MethodPut,
		"/admin/api/v1/projects/"+project.ID+"/keys/"+keyResult.Metadata.ID,
		map[string]any{"name": "service-a", "enabled": false},
	)
	disable.AddCookie(cookie)
	disable.Header.Set("X-CSRF-Token", csrf)
	disable.Header.Set("If-Match", `"1"`)
	disableResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(disableResponse, disable)
	if disableResponse.Code != http.StatusOK || disableResponse.Header().Get("ETag") != `"2"` {
		t.Fatalf("key disable status=%d body=%s", disableResponse.Code, disableResponse.Body.String())
	}
	if _, err := runtime.auth.Authenticate(keyResult.Key, time.Now()); err != auth.ErrKeyDisabled {
		t.Fatalf("disabled key auth error=%v", err)
	}

	deleteProject := adminRequest(t, http.MethodDelete, "/admin/api/v1/projects/"+project.ID, nil)
	deleteProject.AddCookie(cookie)
	deleteProject.Header.Set("X-CSRF-Token", csrf)
	missingRevision := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(missingRevision, deleteProject)
	if missingRevision.Code != http.StatusPreconditionRequired {
		t.Fatalf("delete without If-Match status=%d", missingRevision.Code)
	}
	deleteProject = adminRequest(t, http.MethodDelete, "/admin/api/v1/projects/"+project.ID, nil)
	deleteProject.AddCookie(cookie)
	deleteProject.Header.Set("X-CSRF-Token", csrf)
	deleteProject.Header.Set("If-Match", `"1"`)
	deleteResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(deleteResponse, deleteProject)
	if deleteResponse.Code != http.StatusNoContent || deleteResponse.Header().Get("ETag") != `"2"` {
		t.Fatalf("project delete status=%d body=%s", deleteResponse.Code, deleteResponse.Body.String())
	}
}

func loginAdminForTest(t *testing.T, runtime *Runtime) (*http.Cookie, string) {
	t.Helper()
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, adminRequest(
		t, http.MethodPost, "/admin/api/v1/session/login",
		map[string]string{"username": "admin", "password": "correct horse battery staple"},
	))
	if response.Code != http.StatusOK {
		t.Fatalf("admin login status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return response.Result().Cookies()[0], body.CSRF
}
