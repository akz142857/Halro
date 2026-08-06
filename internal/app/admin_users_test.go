package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

type loggedInAdmin struct {
	cookie *http.Cookie
	csrf   string
}

func loginTestAdmin(t *testing.T, runtime *Runtime, username, password string) loggedInAdmin {
	t.Helper()
	login := adminRequest(t, http.MethodPost, "/admin/api/v1/session/login", map[string]string{
		"username": username, "password": password,
	})
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, login)
	if response.Code != http.StatusOK {
		t.Fatalf("login(%s) status=%d body=%s", username, response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login(%s) cookies=%#v", username, cookies)
	}
	var body struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return loggedInAdmin{cookie: cookies[0], csrf: body.CSRFToken}
}

func adminMutationRequest(t *testing.T, method, path string, session loggedInAdmin, body any) *http.Request {
	t.Helper()
	request := adminRequest(t, method, path, body)
	request.AddCookie(session.cookie)
	request.Header.Set("X-CSRF-Token", session.csrf)
	return request
}

func TestCreateAdminUserRequiresStepUpAndProducesAWorkingReadOnlyLogin(t *testing.T) {
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
	session := loginTestAdmin(t, runtime, "admin", adminPassword)

	// Wrong step-up password is rejected before the user is ever created.
	wrongPassword := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/admin-users", session, map[string]string{
		"username": "viewer", "password": "another correct horse battery staple", "role": "read_only",
		"current_password": "not the admin password",
	})
	wrongPasswordResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(wrongPasswordResponse, wrongPassword)
	if wrongPasswordResponse.Code != http.StatusUnauthorized {
		t.Fatalf("step-up with wrong password status=%d body=%s", wrongPasswordResponse.Code, wrongPasswordResponse.Body.String())
	}

	create := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/admin-users", session, map[string]string{
		"username": "viewer", "password": "another correct horse battery staple", "role": "read_only",
		"current_password": adminPassword,
	})
	createResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createResponse.Code, createResponse.Body.String())
	}
	var created adminUserView
	if err := json.Unmarshal(createResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Username != "viewer" || created.Role != "read_only" {
		t.Fatalf("created=%#v", created)
	}

	// An invalid role is rejected outright.
	invalidRole := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/admin-users", session, map[string]string{
		"username": "someone_else", "password": "another correct horse battery staple", "role": "superuser",
		"current_password": adminPassword,
	})
	invalidRoleResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(invalidRoleResponse, invalidRole)
	if invalidRoleResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid role status=%d", invalidRoleResponse.Code)
	}

	// The new account can actually log in.
	viewerSession := loginTestAdmin(t, runtime, "viewer", "another correct horse battery staple")
	list := adminRequest(t, http.MethodGet, "/admin/api/v1/admin-users", nil)
	list.AddCookie(viewerSession.cookie)
	listResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(listResponse, list)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("read_only list status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}
	var listing struct {
		Users []adminUserView `json:"users"`
	}
	if err := json.Unmarshal(listResponse.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Users) != 2 {
		t.Fatalf("users=%#v", listing.Users)
	}
}

func TestDeleteAdminUserRejectsSelfAndLastAdministrator(t *testing.T) {
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
	session := loginTestAdmin(t, runtime, "admin", adminPassword)

	selfDelete := adminMutationRequest(t, http.MethodDelete, "/admin/api/v1/admin-users/admin", session, map[string]string{
		"current_password": adminPassword,
	})
	selfDeleteResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(selfDeleteResponse, selfDelete)
	if selfDeleteResponse.Code != http.StatusBadRequest {
		t.Fatalf("self-delete status=%d body=%s", selfDeleteResponse.Code, selfDeleteResponse.Body.String())
	}

	create := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/admin-users", session, map[string]string{
		"username": "second_admin", "password": "another correct horse battery staple", "role": "administrator",
		"current_password": adminPassword,
	})
	createResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create second administrator status=%d body=%s", createResponse.Code, createResponse.Body.String())
	}

	// With two administrators, deleting one succeeds.
	deleteSecond := adminMutationRequest(t, http.MethodDelete, "/admin/api/v1/admin-users/second_admin", session, map[string]string{
		"current_password": adminPassword,
	})
	deleteSecondResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(deleteSecondResponse, deleteSecond)
	if deleteSecondResponse.Code != http.StatusNoContent {
		t.Fatalf("delete second administrator status=%d body=%s", deleteSecondResponse.Code, deleteSecondResponse.Body.String())
	}

	// Now "admin" is the only administrator left; create a read_only user and
	// confirm the LAST administrator cannot be removed even by another caller.
	createViewer := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/admin-users", session, map[string]string{
		"username": "viewer", "password": "another correct horse battery staple", "role": "read_only",
		"current_password": adminPassword,
	})
	createViewerResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(createViewerResponse, createViewer)
	if createViewerResponse.Code != http.StatusCreated {
		t.Fatalf("create viewer status=%d", createViewerResponse.Code)
	}
	viewerSession := loginTestAdmin(t, runtime, "viewer", "another correct horse battery staple")
	deleteLastAdmin := adminMutationRequest(t, http.MethodDelete, "/admin/api/v1/admin-users/admin", viewerSession, map[string]string{
		"current_password": "another correct horse battery staple",
	})
	deleteLastAdminResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(deleteLastAdminResponse, deleteLastAdmin)
	// A read_only caller never reaches the "last administrator" check at all —
	// the role gate on requireAdminMutation refuses it first.
	if deleteLastAdminResponse.Code != http.StatusForbidden {
		t.Fatalf("read_only delete attempt status=%d body=%s", deleteLastAdminResponse.Code, deleteLastAdminResponse.Body.String())
	}
}

// TestReadOnlyRoleCannotReachAnyRegisteredMutationRoute is a table-driven
// sweep across every mutation route the admin router has registered, rather
// than a maintained per-endpoint list — the two-tier RBAC decision
// (docs/review/260805/progress.md P2-23) was specifically to avoid a
// permission matrix that a new write endpoint could be added without
// updating. Routes
// that act on the caller's own account (logout, own password, own MFA, own
// preferences, listing users) are the deliberate, narrow exception.
func TestReadOnlyRoleCannotReachAnyRegisteredMutationRoute(t *testing.T) {
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
	admin := loginTestAdmin(t, runtime, "admin", adminPassword)
	create := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/admin-users", admin, map[string]string{
		"username": "viewer", "password": "another correct horse battery staple", "role": "read_only",
		"current_password": adminPassword,
	})
	createResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create viewer status=%d body=%s", createResponse.Code, createResponse.Body.String())
	}
	viewer := loginTestAdmin(t, runtime, "viewer", "another correct horse battery staple")

	selfService := map[string]bool{
		"POST /admin/api/v1/session/logout":                           true,
		"POST /admin/api/v1/session/password":                         true,
		"POST /admin/api/v1/security/mfa/authenticators":              true,
		"POST /admin/api/v1/security/mfa/authenticators/{}/confirm":   true,
		"DELETE /admin/api/v1/security/mfa/authenticators/{}/pending": true,
		"PATCH /admin/api/v1/security/mfa/authenticators/{}":          true,
		"DELETE /admin/api/v1/security/mfa/authenticators/{}":         true,
		"POST /admin/api/v1/security/mfa/recovery-codes/regenerate":   true,
		"DELETE /admin/api/v1/security/mfa":                           true,
		"PUT /admin/api/v1/preferences":                               true,
		// Setup and login happen before any session exists — not part of the
		// authenticated mutation surface this test is sweeping.
		"POST /admin/api/v1/setup/admin":               true,
		"POST /admin/api/v1/session/login":             true,
		"POST /admin/api/v1/session/mfa/totp":          true,
		"POST /admin/api/v1/session/mfa/recovery-code": true,
		"DELETE /admin/api/v1/session/mfa/challenge":   true,
	}
	parameter := regexp.MustCompile(`\{[^}]+\}`)
	routes, ok := runtime.adminRouter().(chi.Routes)
	if !ok {
		t.Fatal("admin router does not expose chi route metadata")
	}
	tested := 0
	if err := chi.Walk(routes, func(method, path string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
			return nil
		}
		if !strings.HasPrefix(path, "/admin/api/") {
			// chi.Walk also surfaces the mount point's catch-all
			// 404/405 stub handlers ("/admin", "/admin/*"), which are
			// not real registered endpoints.
			return nil
		}
		key := method + " " + parameter.ReplaceAllString(path, "{}")
		if selfService[key] {
			return nil
		}
		tested++
		concretePath := parameter.ReplaceAllString(path, "placeholder")
		request := adminMutationRequest(t, method, concretePath, viewer, map[string]string{})
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Errorf("%s %s: read_only reached the handler, status=%d body=%s", method, path, response.Code, response.Body.String())
			return nil
		}
		var body struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Code != "read_only_role" {
			t.Errorf("%s %s: 403 was not the role gate (code=%q, want read_only_role)", method, path, body.Code)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if tested < 40 {
		t.Fatalf("only swept %d mutation routes — the walk likely missed most of adminRouter()", tested)
	}
}
