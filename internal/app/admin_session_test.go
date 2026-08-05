package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/audit"
	"github.com/akz142857/Heimdall/internal/domain"
)

func TestAdminBootstrapLoginCSRFAndLogout(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	password := []byte("correct horse battery staple")
	if err := BootstrapAdmin(context.Background(), cfg, "admin", password); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "other", password); err == nil {
		t.Fatal("second admin bootstrap must fail")
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	login := adminRequest(t, http.MethodPost, "/admin/api/v1/session/login", map[string]string{
		"username": "admin", "password": string(password),
	})
	loginResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(loginResponse, login)
	if loginResponse.Code != http.StatusOK {
		runtime.Close()
		t.Fatalf("login status=%d body=%s", loginResponse.Code, loginResponse.Body.String())
	}
	cookies := loginResponse.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != adminSessionCookie ||
		!cookies[0].Secure || !cookies[0].HttpOnly ||
		cookies[0].SameSite != http.SameSiteStrictMode {
		runtime.Close()
		t.Fatalf("invalid session cookie: %#v", cookies)
	}
	var loginBody struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(loginResponse.Body.Bytes(), &loginBody); err != nil {
		runtime.Close()
		t.Fatal(err)
	}
	if loginBody.CSRFToken == "" {
		runtime.Close()
		t.Fatal("login did not return a CSRF token")
	}

	sessionRequest := adminRequest(t, http.MethodGet, "/admin/api/v1/session", nil)
	sessionRequest.AddCookie(cookies[0])
	sessionResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(sessionResponse, sessionRequest)
	if sessionResponse.Code != http.StatusOK ||
		!bytes.Contains(sessionResponse.Body.Bytes(), []byte(loginBody.CSRFToken)) {
		runtime.Close()
		t.Fatalf("session status=%d body=%s", sessionResponse.Code, sessionResponse.Body.String())
	}

	rejectedLogout := adminRequest(t, http.MethodPost, "/admin/api/v1/session/logout", nil)
	rejectedLogout.AddCookie(cookies[0])
	rejectedResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(rejectedResponse, rejectedLogout)
	if rejectedResponse.Code != http.StatusForbidden {
		runtime.Close()
		t.Fatalf("logout without CSRF status=%d", rejectedResponse.Code)
	}

	logout := adminRequest(t, http.MethodPost, "/admin/api/v1/session/logout", nil)
	logout.AddCookie(cookies[0])
	logout.Header.Set("X-CSRF-Token", loginBody.CSRFToken)
	logoutResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(logoutResponse, logout)
	if logoutResponse.Code != http.StatusOK {
		runtime.Close()
		t.Fatalf("logout status=%d body=%s", logoutResponse.Code, logoutResponse.Body.String())
	}
	if value := logoutResponse.Header().Get("Clear-Site-Data"); value != "" {
		runtime.Close()
		t.Fatalf("logout must not trigger synchronous origin clearing, got %q", value)
	}
	logoutCookies := logoutResponse.Result().Cookies()
	if len(logoutCookies) != 1 || logoutCookies[0].Name != adminSessionCookie ||
		logoutCookies[0].Value != "" || logoutCookies[0].MaxAge >= 0 ||
		!logoutCookies[0].HttpOnly || !logoutCookies[0].Secure {
		runtime.Close()
		t.Fatalf("logout did not expire the secure session cookie: %#v", logoutCookies)
	}

	expiredRequest := adminRequest(t, http.MethodGet, "/admin/api/v1/session", nil)
	expiredRequest.AddCookie(cookies[0])
	expiredResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(expiredResponse, expiredRequest)
	if expiredResponse.Code != http.StatusUnauthorized {
		runtime.Close()
		t.Fatalf("revoked session status=%d", expiredResponse.Code)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	summary, err := VerifyAudit(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Records < 5 {
		t.Fatalf("expected bootstrap/start/login/logout/shutdown audit events, got %d", summary.Records)
	}
}

func TestAdminPasswordChangeRotatesEverySession(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	oldPassword := "correct horse battery staple"
	newPassword := "a newer and stronger password"
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte(oldPassword)); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	user, err := runtime.store.GetAdminUser(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	user.Appearance = domain.AppearanceLight
	if _, err := runtime.store.PutAdminUser(context.Background(), user, user.Revision); err != nil {
		t.Fatal(err)
	}
	loginResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(loginResponse, adminRequest(
		t, http.MethodPost, "/admin/api/v1/session/login",
		map[string]string{"username": "admin", "password": oldPassword},
	))
	var loginBody struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(loginResponse.Body.Bytes(), &loginBody); err != nil {
		t.Fatal(err)
	}
	oldCookie := loginResponse.Result().Cookies()[0]
	change := adminRequest(t, http.MethodPost, "/admin/api/v1/session/password", map[string]string{
		"current_password": oldPassword, "new_password": newPassword,
	})
	change.AddCookie(oldCookie)
	change.Header.Set("X-CSRF-Token", loginBody.CSRFToken)
	changeResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(changeResponse, change)
	if changeResponse.Code != http.StatusOK {
		t.Fatalf("password change status=%d body=%s", changeResponse.Code, changeResponse.Body.String())
	}
	if !jsonBodyContains(t, changeResponse, `"appearance":"light"`) {
		t.Fatalf("password change did not preserve appearance: %s", changeResponse.Body.String())
	}
	newCookie := changeResponse.Result().Cookies()[0]
	if newCookie.Value == oldCookie.Value {
		t.Fatal("password change did not rotate the session")
	}
	oldSession := adminRequest(t, http.MethodGet, "/admin/api/v1/session", nil)
	oldSession.AddCookie(oldCookie)
	oldSessionResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(oldSessionResponse, oldSession)
	if oldSessionResponse.Code != http.StatusUnauthorized {
		t.Fatalf("old session survived password change: %d", oldSessionResponse.Code)
	}
	oldLoginResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(oldLoginResponse, adminRequest(
		t, http.MethodPost, "/admin/api/v1/session/login",
		map[string]string{"username": "admin", "password": oldPassword},
	))
	if oldLoginResponse.Code != http.StatusUnauthorized {
		t.Fatalf("old password survived change: %d", oldLoginResponse.Code)
	}
	newLoginResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(newLoginResponse, adminRequest(
		t, http.MethodPost, "/admin/api/v1/session/login",
		map[string]string{"username": "admin", "password": newPassword},
	))
	if newLoginResponse.Code != http.StatusOK {
		t.Fatalf("new password login status=%d body=%s", newLoginResponse.Code, newLoginResponse.Body.String())
	}
	if !jsonBodyContains(t, newLoginResponse, `"appearance":"light"`) {
		t.Fatalf("new login did not preserve appearance: %s", newLoginResponse.Body.String())
	}
}

func TestAdminReadAPIUsesSecretSafeViews(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	providerSecret := []byte("provider-secret-canary-value")
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test",
		PublicModel: "chat", ProjectName: "Default",
		BillingMode: domain.BillingModeFree,
	}, providerSecret)
	if err != nil {
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
	loginResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(loginResponse, adminRequest(
		t, http.MethodPost, "/admin/api/v1/session/login",
		map[string]string{"username": "admin", "password": "correct horse battery staple"},
	))
	if loginResponse.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", loginResponse.Code, loginResponse.Body.String())
	}
	cookie := loginResponse.Result().Cookies()[0]
	var combined bytes.Buffer
	for _, path := range []string{
		"/admin/api/v1/credentials",
		"/admin/api/v1/providers",
		"/admin/api/v1/projects",
		"/admin/api/v1/projects/" + bootstrap.ProjectID + "/keys",
		"/admin/api/v1/routes",
		"/admin/api/v1/master-key/custody",
	} {
		request := adminRequest(t, http.MethodGet, path, nil)
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
		combined.Write(response.Body.Bytes())
	}
	payload := combined.String()
	for _, forbidden := range []string{
		string(providerSecret), bootstrap.GatewayKey, `"ciphertext"`, `"key_hash"`,
	} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("admin response leaked %q: %s", forbidden, payload)
		}
	}
	if !strings.Contains(payload, `"secret_configured":true`) {
		t.Fatalf("credential view did not report secret presence: %s", payload)
	}
}

// Login is unauthenticated, and every audit append fsyncs and indexes a record.
// If a rejected request can still reach the audit path, anonymous traffic sets
// its own append rate — and because audit failures are fail-closed, filling the
// disk that way takes every administrative action down with it. Rate limiting
// therefore has to run ahead of the origin check rather than behind it.
func TestRejectedLoginSprayCannotSetTheAuditAppendRate(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	before := countLoginAudits(t, runtime)
	const spray = 200
	throttled := 0
	for i := 0; i < spray; i++ {
		request := adminRequest(t, http.MethodPost, "/admin/api/v1/session/login", map[string]string{
			"username": "admin", "password": "wrong",
		})
		request.Header.Set("Origin", "https://attacker.example")
		request.RemoteAddr = "192.0.2.50:9000"
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, request)
		if response.Code == http.StatusTooManyRequests {
			throttled++
		}
	}
	if throttled == 0 {
		t.Fatal("a 200-request spray from one source was never throttled")
	}
	// At most one audited attempt per allowed request, plus a single
	// rate_limited record covering the rest of the minute.
	appended := countLoginAudits(t, runtime) - before
	if ceiling := cfg.Admin.LoginRPM + 1; appended > ceiling {
		t.Fatalf("%d rejected requests appended %d audit records, want at most %d",
			spray, appended, ceiling)
	}
}

// A throttled source still has to be visible in the audit trail; the fix is to
// record it once per window, not to stop recording it.
func TestAdminRateLimiterReportsEachThrottledWindowOnce(t *testing.T) {
	var mu sync.Mutex
	windows := map[string]adminLoginWindow{}
	now := time.Date(2026, 8, 5, 9, 30, 0, 0, time.UTC)
	const limit = 3
	for i := 0; i < limit; i++ {
		if allowed, _ := allowAdminRate(&mu, windows, "203.0.113.7:1234", now, limit); !allowed {
			t.Fatalf("attempt %d rejected below the limit", i+1)
		}
	}
	allowed, report := allowAdminRate(&mu, windows, "203.0.113.7:1234", now, limit)
	if allowed || !report {
		t.Fatalf("first rejection: allowed=%v report=%v, want false/true", allowed, report)
	}
	for i := 0; i < 100; i++ {
		allowed, repeat := allowAdminRate(&mu, windows, "203.0.113.7:1234", now, limit)
		if allowed || repeat {
			t.Fatalf("sustained rejection %d: allowed=%v report=%v, want false/false", i+1, allowed, repeat)
		}
	}
	if allowed, _ := allowAdminRate(&mu, windows, "203.0.113.7:1234", now.Add(time.Minute), limit); !allowed {
		t.Fatal("a new minute did not reset the window")
	}
}

func countLoginAudits(t *testing.T, runtime *Runtime) int {
	t.Helper()
	count := 0
	if _, err := runtime.audit.Replay(func(record audit.Record) error {
		if record.Event.Action == "admin.login" {
			count++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

func adminRequest(
	t *testing.T,
	method string,
	path string,
	body any,
) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, "http://127.0.0.1:18081"+path, reader)
	request.Header.Set("Origin", "http://127.0.0.1:18081")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}
