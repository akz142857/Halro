package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akz142857/Heimdall/internal/domain"
)

func TestAdminUILocaleSettingsAndPreferencesPersist(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtime, err := Open(context.Background(), cfg, logger)
	if err != nil {
		t.Fatal(err)
	}

	bootstrap := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(bootstrap, httptest.NewRequest(http.MethodGet, "/admin/api/v1/ui/bootstrap", nil))
	if bootstrap.Code != http.StatusOK {
		t.Fatalf("bootstrap status=%d body=%s", bootstrap.Code, bootstrap.Body.String())
	}
	var bootstrapBody struct {
		DefaultLocale    string   `json:"default_locale"`
		SupportedLocales []string `json:"supported_locales"`
	}
	if err := json.Unmarshal(bootstrap.Body.Bytes(), &bootstrapBody); err != nil {
		t.Fatal(err)
	}
	if bootstrapBody.DefaultLocale != domain.LocaleZhCN || len(bootstrapBody.SupportedLocales) != 2 {
		t.Fatalf("unexpected bootstrap: %#v", bootstrapBody)
	}
	if bytes.Contains(bootstrap.Body.Bytes(), []byte("revision")) || bytes.Contains(bootstrap.Body.Bytes(), []byte("updated_at")) {
		t.Fatalf("public bootstrap exposed internal metadata: %s", bootstrap.Body.String())
	}
	for _, path := range []string{"/admin/api/v1/preferences", "/admin/api/v1/settings/ui"} {
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous GET %s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}

	cookie, csrf := loginAdminForTest(t, runtime)
	withoutCSRF := adminRequest(t, http.MethodPut, "/admin/api/v1/preferences", map[string]string{"locale": domain.LocaleEnUS})
	withoutCSRF.AddCookie(cookie)
	withoutCSRF.Header.Set("If-Match", `"1"`)
	withoutCSRFResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(withoutCSRFResponse, withoutCSRF)
	if withoutCSRFResponse.Code != http.StatusForbidden {
		t.Fatalf("preference update without CSRF status=%d body=%s", withoutCSRFResponse.Code, withoutCSRFResponse.Body.String())
	}
	missingRevision := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/preferences", "", map[string]string{"locale": domain.LocaleEnUS})
	if missingRevision.Code != http.StatusPreconditionRequired {
		t.Fatalf("preference update without If-Match status=%d body=%s", missingRevision.Code, missingRevision.Body.String())
	}
	preferences := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/preferences")
	if preferences.Code != http.StatusOK || preferences.Header().Get("ETag") != `"1"` {
		t.Fatalf("preferences status=%d etag=%q body=%s", preferences.Code, preferences.Header().Get("ETag"), preferences.Body.String())
	}
	preferenceUpdate := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/preferences", `"1"`, map[string]string{"locale": domain.LocaleEnUS})
	if preferenceUpdate.Code != http.StatusOK || preferenceUpdate.Header().Get("ETag") != `"2"` {
		t.Fatalf("preference update status=%d etag=%q body=%s", preferenceUpdate.Code, preferenceUpdate.Header().Get("ETag"), preferenceUpdate.Body.String())
	}
	invalidPreference := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/preferences", `"2"`, map[string]string{"locale": "xx-YY"})
	if invalidPreference.Code != http.StatusBadRequest {
		t.Fatalf("invalid preference status=%d body=%s", invalidPreference.Code, invalidPreference.Body.String())
	}

	uiSettings := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/settings/ui")
	if uiSettings.Code != http.StatusOK || uiSettings.Header().Get("ETag") != `"1"` {
		t.Fatalf("UI settings status=%d etag=%q body=%s", uiSettings.Code, uiSettings.Header().Get("ETag"), uiSettings.Body.String())
	}
	uiUpdate := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/settings/ui", `"1"`, map[string]string{"default_locale": domain.LocaleEnUS})
	if uiUpdate.Code != http.StatusOK || uiUpdate.Header().Get("ETag") != `"2"` {
		t.Fatalf("UI update status=%d etag=%q body=%s", uiUpdate.Code, uiUpdate.Header().Get("ETag"), uiUpdate.Body.String())
	}
	staleUIUpdate := performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/settings/ui", `"1"`, map[string]string{"default_locale": domain.LocaleZhCN})
	if staleUIUpdate.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale UI update status=%d body=%s", staleUIUpdate.Code, staleUIUpdate.Body.String())
	}

	session := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/session")
	if session.Code != http.StatusOK || !jsonBodyContains(t, session, `"locale":"en-US"`) {
		t.Fatalf("session did not expose locale: status=%d body=%s", session.Code, session.Body.String())
	}
	audit := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/audit")
	if audit.Code != http.StatusOK ||
		!bytes.Contains(audit.Body.Bytes(), []byte("admin.preferences.update")) ||
		!bytes.Contains(audit.Body.Bytes(), []byte("settings.ui.update")) {
		t.Fatalf("locale mutations were not audited: status=%d body=%s", audit.Code, audit.Body.String())
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(context.Background(), cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	storedUI := reopened.uiSettings.Load()
	if storedUI == nil || storedUI.DefaultLocale != domain.LocaleEnUS || storedUI.Revision != 2 {
		t.Fatalf("UI settings were not persisted: %#v", storedUI)
	}
	storedUser, err := reopened.store.GetAdminUser(context.Background(), "admin")
	if err != nil || storedUser.Locale != domain.LocaleEnUS {
		t.Fatalf("admin preference was not persisted: %#v err=%v", storedUser, err)
	}
}

func authenticatedAdminGet(t *testing.T, runtime *Runtime, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, request)
	return response
}

func jsonBodyContains(t *testing.T, response *httptest.ResponseRecorder, expected string) bool {
	t.Helper()
	var value any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	return err == nil && bytes.Contains(encoded, []byte(expected))
}
