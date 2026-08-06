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
	seedRouteForTest(t, runtime, "chat")

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

	withoutIdempotency := adminRequest(t, http.MethodPost,
		"/admin/api/v1/projects/"+project.ID+"/keys",
		map[string]any{"name": "service-a"},
	)
	withoutIdempotency.AddCookie(cookie)
	withoutIdempotency.Header.Set("X-CSRF-Token", csrf)
	withoutIdempotencyResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(withoutIdempotencyResponse, withoutIdempotency)
	if withoutIdempotencyResponse.Code != http.StatusBadRequest ||
		!strings.Contains(withoutIdempotencyResponse.Body.String(), "idempotency_key_required") {
		t.Fatalf("key create without Idempotency-Key status=%d body=%s",
			withoutIdempotencyResponse.Code, withoutIdempotencyResponse.Body.String())
	}

	createKey := adminRequest(t, http.MethodPost,
		"/admin/api/v1/projects/"+project.ID+"/keys",
		map[string]any{"name": "service-a"},
	)
	createKey.AddCookie(cookie)
	createKey.Header.Set("X-CSRF-Token", csrf)
	createKey.Header.Set("Idempotency-Key", "service-a-1")
	createKeyResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(createKeyResponse, createKey)
	if createKeyResponse.Code != http.StatusCreated ||
		createKeyResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("key create status=%d body=%s", createKeyResponse.Code, createKeyResponse.Body.String())
	}

	// A retried create must never mint a second live credential: the plaintext of the
	// first one is already gone, so a duplicate would be an unaccounted-for key.
	replay := adminRequest(t, http.MethodPost,
		"/admin/api/v1/projects/"+project.ID+"/keys",
		map[string]any{"name": "service-a"},
	)
	replay.AddCookie(cookie)
	replay.Header.Set("X-CSRF-Token", csrf)
	replay.Header.Set("Idempotency-Key", "service-a-1")
	replayResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(replayResponse, replay)
	if replayResponse.Code != http.StatusConflict ||
		!strings.Contains(replayResponse.Body.String(), "gateway_key_idempotency_replay") {
		t.Fatalf("key create replay status=%d body=%s", replayResponse.Code, replayResponse.Body.String())
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
	deleteProject = adminRequest(t, http.MethodDelete, "/admin/api/v1/projects/"+project.ID, stepUp())
	deleteProject.AddCookie(cookie)
	deleteProject.Header.Set("X-CSRF-Token", csrf)
	deleteProject.Header.Set("If-Match", `"1"`)
	deleteResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(deleteResponse, deleteProject)
	if deleteResponse.Code != http.StatusNoContent || deleteResponse.Header().Get("ETag") != `"2"` {
		t.Fatalf("project delete status=%d body=%s", deleteResponse.Code, deleteResponse.Body.String())
	}
}

// seedRouteForTest publishes a public model alias so projects may reference it.
// validateProjectReferences rejects aliases with no route behind them, and a route in
// turn needs a provider, so both records go in.
func seedRouteForTest(t *testing.T, runtime *Runtime, publicModel string) {
	t.Helper()
	now := time.Now().UTC()
	profile, _ := domain.DefaultProviderProfile(domain.ProviderOpenAI)
	capabilities := domain.DefaultProviderCapabilities(domain.ProviderOpenAI)
	if _, err := runtime.store.PutCredential(context.Background(), domain.Credential{
		ID: "cred_route_seed", Name: "Seed", Type: domain.ProviderOpenAI,
		Audience: "https://api.openai.com:443:openai", Ciphertext: []byte("sealed"),
		KeyVersion: 1, CreatedAt: now, UpdatedAt: now,
	}, 0); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatal(err)
	}
	if _, err := runtime.store.PutProvider(context.Background(), domain.ProviderInstance{
		ID: "provider_route_seed", Name: "Seed", Type: domain.ProviderOpenAI,
		BaseURL: "https://api.openai.com", AllowedHosts: []string{"api.openai.com"},
		CredentialID: "cred_route_seed", Enabled: true, CreatedAt: now, UpdatedAt: now,
		AccessSurface: profile.AccessSurface, ProfileID: profile.ProfileID,
		CredentialScheme: profile.CredentialScheme, Capabilities: capabilities,
		CapabilityEvidence: domain.EvidenceForCapabilities(capabilities, domain.EvidenceDeclared),
	}, 0); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatal(err)
	}
	if _, err := runtime.store.PutRoute(context.Background(), domain.Route{
		ID: "rt_" + publicModel, PublicModel: publicModel, ProviderID: "provider_route_seed",
		ProviderModel: "upstream-" + publicModel, Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}, 0); err != nil {
		t.Fatal(err)
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

// A project created without allowed_routes must still serialise the field as an array.
// Clients iterate it directly, and a JSON null used to blank the admin console.
func TestAdminProjectOmittingAllowedRoutesSerialisesEmptyArray(t *testing.T) {
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

	create := adminRequest(t, http.MethodPost, "/admin/api/v1/projects", map[string]any{
		"name": "NoRoutes", "enabled": true,
		"rpm": int64(60), "tpm": int64(100_000), "max_concurrency": int64(8),
		"daily_budget_micros_usd":     int64(5_000_000),
		"max_input_tokens":            int64(32_000),
		"max_output_tokens":           int64(4_096),
		"max_request_bytes":           int64(1 << 20),
		"max_stream_duration_seconds": int64(120),
	})
	create.AddCookie(cookie)
	create.Header.Set("X-CSRF-Token", csrf)
	createResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("project create status=%d body=%s", createResponse.Code, createResponse.Body.String())
	}
	if strings.Contains(createResponse.Body.String(), `"allowed_routes":null`) {
		t.Fatalf("create response carried a null allowed_routes: %s", createResponse.Body.String())
	}

	list := adminRequest(t, http.MethodGet, "/admin/api/v1/projects", nil)
	list.AddCookie(cookie)
	listResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(listResponse, list)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("project list status=%d", listResponse.Code)
	}
	if strings.Contains(listResponse.Body.String(), `"allowed_routes":null`) {
		t.Fatalf("list response carried a null allowed_routes: %s", listResponse.Body.String())
	}
	var page struct {
		Items []struct {
			AllowedRoutes []string `json:"allowed_routes"`
		} `json:"items"`
	}
	if err := json.Unmarshal(listResponse.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].AllowedRoutes == nil {
		t.Fatalf("expected one project with a non-nil allowed_routes, got %+v", page.Items)
	}
}

// Deleting a gateway key must revoke it on the request path, not merely hide it from the
// console: a leaked credential has to stop authenticating the moment the operator acts.
func TestAdminGatewayKeyDeleteRevokesImmediately(t *testing.T) {
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
	seedRouteForTest(t, runtime, "chat")
	project := createProjectForTest(t, runtime, cookie, csrf, "Revocation", []string{"chat"})

	createKey := adminRequest(t, http.MethodPost,
		"/admin/api/v1/projects/"+project.ID+"/keys", map[string]any{"name": "doomed"})
	createKey.AddCookie(cookie)
	createKey.Header.Set("X-CSRF-Token", csrf)
	createKey.Header.Set("Idempotency-Key", "doomed-1")
	createKeyResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(createKeyResponse, createKey)
	if createKeyResponse.Code != http.StatusCreated {
		t.Fatalf("key create status=%d body=%s", createKeyResponse.Code, createKeyResponse.Body.String())
	}
	var keyResult struct {
		Key      string         `json:"key"`
		Metadata gatewayKeyView `json:"metadata"`
	}
	if err := json.Unmarshal(createKeyResponse.Body.Bytes(), &keyResult); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.auth.Authenticate(keyResult.Key, time.Now()); err != nil {
		t.Fatalf("new key is not active: %v", err)
	}

	keyPath := "/admin/api/v1/projects/" + project.ID + "/keys/" + keyResult.Metadata.ID
	missingRevision := adminRequest(t, http.MethodDelete, keyPath, nil)
	missingRevision.AddCookie(cookie)
	missingRevision.Header.Set("X-CSRF-Token", csrf)
	missingRevisionResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(missingRevisionResponse, missingRevision)
	if missingRevisionResponse.Code != http.StatusPreconditionRequired {
		t.Fatalf("key delete without If-Match status=%d", missingRevisionResponse.Code)
	}

	remove := adminRequest(t, http.MethodDelete, keyPath, stepUp())
	remove.AddCookie(cookie)
	remove.Header.Set("X-CSRF-Token", csrf)
	remove.Header.Set("If-Match", `"1"`)
	removeResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(removeResponse, remove)
	if removeResponse.Code != http.StatusNoContent {
		t.Fatalf("key delete status=%d body=%s", removeResponse.Code, removeResponse.Body.String())
	}
	if _, err := runtime.auth.Authenticate(keyResult.Key, time.Now()); err == nil {
		t.Fatal("deleted key still authenticates")
	}

	read := adminRequest(t, http.MethodGet, keyPath, nil)
	read.AddCookie(cookie)
	readResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(readResponse, read)
	if readResponse.Code != http.StatusNotFound {
		t.Fatalf("deleted key read status=%d body=%s", readResponse.Code, readResponse.Body.String())
	}

	update := adminRequest(t, http.MethodPut, keyPath, map[string]any{"name": "doomed", "enabled": true})
	update.AddCookie(cookie)
	update.Header.Set("X-CSRF-Token", csrf)
	update.Header.Set("If-Match", `"2"`)
	updateResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(updateResponse, update)
	if updateResponse.Code != http.StatusNotFound {
		t.Fatalf("deleted key update status=%d body=%s", updateResponse.Code, updateResponse.Body.String())
	}

	list := adminRequest(t, http.MethodGet, "/admin/api/v1/projects/"+project.ID+"/keys", nil)
	list.AddCookie(cookie)
	listResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(listResponse, list)
	if listResponse.Code != http.StatusOK || strings.Contains(listResponse.Body.String(), keyResult.Metadata.ID) {
		t.Fatalf("deleted key still listed status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}
}

// domain.GatewayKey is the persistence model and carries KeyHash, so every admin
// response has to serialise gatewayKeyView instead. Assert that on all key surfaces.
func TestAdminKeyResponsesNeverExposeKeyHash(t *testing.T) {
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
	seedRouteForTest(t, runtime, "chat")
	project := createProjectForTest(t, runtime, cookie, csrf, "Hashes", []string{"chat"})

	createKey := adminRequest(t, http.MethodPost,
		"/admin/api/v1/projects/"+project.ID+"/keys", map[string]any{"name": "reader"})
	createKey.AddCookie(cookie)
	createKey.Header.Set("X-CSRF-Token", csrf)
	createKey.Header.Set("Idempotency-Key", "reader-1")
	createKeyResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(createKeyResponse, createKey)
	if createKeyResponse.Code != http.StatusCreated {
		t.Fatalf("key create status=%d body=%s", createKeyResponse.Code, createKeyResponse.Body.String())
	}
	var keyResult struct {
		Metadata gatewayKeyView `json:"metadata"`
	}
	if err := json.Unmarshal(createKeyResponse.Body.Bytes(), &keyResult); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/admin/api/v1/projects/" + project.ID + "/keys",
		"/admin/api/v1/projects/" + project.ID + "/keys/" + keyResult.Metadata.ID,
	} {
		request := adminRequest(t, http.MethodGet, path, nil)
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d", path, response.Code)
		}
		if strings.Contains(response.Body.String(), "key_hash") ||
			strings.Contains(response.Body.String(), "hash_version") {
			t.Fatalf("%s leaked the key hash: %s", path, response.Body.String())
		}
	}
}

// An alias with no route behind it silently rejects every request at the gateway. Fail
// the write instead, while the operator is still looking at the form.
func TestAdminProjectRejectsUnknownModelAlias(t *testing.T) {
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
	seedRouteForTest(t, runtime, "chat")

	create := adminRequest(t, http.MethodPost, "/admin/api/v1/projects", map[string]any{
		"name": "Typo", "enabled": true, "allowed_routes": []string{"chat", "chatt"},
		"rpm": int64(60), "tpm": int64(100_000), "max_concurrency": int64(8),
	})
	create.AddCookie(cookie)
	create.Header.Set("X-CSRF-Token", csrf)
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, create)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "chatt") {
		t.Fatalf("unknown alias status=%d body=%s", response.Code, response.Body.String())
	}
}

// The web form caps the name at domain.MaxProjectNameLength. Any other caller must hit
// the same wall, so the limit lives in Validate rather than only in the browser.
func TestAdminProjectRejectsOverlongName(t *testing.T) {
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

	create := adminRequest(t, http.MethodPost, "/admin/api/v1/projects", map[string]any{
		"name": strings.Repeat("n", domain.MaxProjectNameLength+1), "enabled": true,
		"rpm": int64(60), "tpm": int64(100_000), "max_concurrency": int64(8),
	})
	create.AddCookie(cookie)
	create.Header.Set("X-CSRF-Token", csrf)
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, create)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "too long") {
		t.Fatalf("overlong name status=%d body=%s", response.Code, response.Body.String())
	}
}

func createProjectForTest(
	t *testing.T, runtime *Runtime, cookie *http.Cookie, csrf, name string, routes []string,
) domain.Project {
	t.Helper()
	create := adminRequest(t, http.MethodPost, "/admin/api/v1/projects", map[string]any{
		"name": name, "enabled": true, "allowed_routes": routes,
		"rpm": int64(60), "tpm": int64(100_000), "max_concurrency": int64(8),
		"daily_budget_micros_usd":     int64(5_000_000),
		"max_input_tokens":            int64(32_000),
		"max_output_tokens":           int64(4_096),
		"max_request_bytes":           int64(1 << 20),
		"max_stream_duration_seconds": int64(120),
	})
	create.AddCookie(cookie)
	create.Header.Set("X-CSRF-Token", csrf)
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, create)
	if response.Code != http.StatusCreated {
		t.Fatalf("project create status=%d body=%s", response.Code, response.Body.String())
	}
	var project domain.Project
	if err := json.Unmarshal(response.Body.Bytes(), &project); err != nil {
		t.Fatal(err)
	}
	return project
}
