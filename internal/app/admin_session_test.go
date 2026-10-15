package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/adminauth"
	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/domain"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
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

func TestAdminBootstrapOperationIsIdempotentWithoutReplacingPassword(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	operation := "install-production-20260917"
	oldPassword := []byte("correct horse battery staple")
	newPassword := []byte("a different correct horse battery staple")
	result, err := BootstrapAdminWithOptions(ctx, cfg, "admin", oldPassword, BootstrapAdminOptions{
		OperationID: operation, IfNeeded: true,
	})
	if err != nil || result != BootstrapAdminCreated {
		t.Fatalf("first result=%q err=%v", result, err)
	}
	result, err = BootstrapAdminWithOptions(ctx, cfg, "admin", newPassword, BootstrapAdminOptions{
		OperationID: operation, IfNeeded: true,
	})
	if err != nil || result != BootstrapAdminAlreadyCompleted {
		t.Fatalf("retry result=%q err=%v", result, err)
	}
	if _, err := BootstrapAdminWithOptions(ctx, cfg, "admin", newPassword, BootstrapAdminOptions{
		OperationID: "another-install", IfNeeded: true,
	}); !errors.Is(err, ErrAdminBootstrapConflict) {
		t.Fatalf("different operation id error=%v", err)
	}
	if _, err := BootstrapAdminWithOptions(ctx, cfg, "other", newPassword, BootstrapAdminOptions{
		OperationID: operation, IfNeeded: true,
	}); !errors.Is(err, ErrAdminBootstrapConflict) {
		t.Fatalf("different username error=%v", err)
	}

	runtime, err := Open(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	login := func(password []byte) int {
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, adminRequest(t, http.MethodPost,
			"/admin/api/v1/session/login", map[string]string{"username": "admin", "password": string(password)}))
		return response.Code
	}
	if status := login(oldPassword); status != http.StatusOK {
		t.Fatalf("original password status=%d", status)
	}
	if status := login(newPassword); status != http.StatusUnauthorized {
		t.Fatalf("retry password status=%d", status)
	}
}

func TestAdminBootstrapReplayFailsWhenCompletionLostItsAuditEvidence(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	user, err := adminauth.NewUser("admin", []byte("correct horse battery staple"), domain.AdminRoleAdministrator, now)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	defer clear(user.PasswordHash)
	defer clear(user.PasswordSalt)
	intent := domain.AdminAuditIntent{
		EventID: "aud_missing_bootstrap_evidence", OccurredAt: now,
		ActorType: "local_cli", ActorID: "admin", Action: "admin.bootstrap",
		TargetType: "admin_user", TargetID: "admin",
		Metadata: map[string]string{"operation_id": "install-missing-audit", "source": "offline"},
	}
	completion := boltstore.AdminBootstrapCompletion{
		OperationID: "install-missing-audit", Username: "admin",
		AuditEventID: intent.EventID, CommittedAt: now,
	}
	if _, _, created, err := store.CreateFirstAdminWithAuditIntent(ctx, user, completion, intent); err != nil || !created {
		store.Close()
		t.Fatalf("created=%v err=%v", created, err)
	}
	// Simulate corruption/unsupported manual intervention after the atomic
	// metadata commit: neither the pending intent nor its audit record remains.
	if err := store.DeleteAdminAuditIntent(ctx, intent.EventID); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := BootstrapAdminWithOptions(ctx, cfg, "admin", []byte("a different correct horse battery staple"), BootstrapAdminOptions{
		OperationID: completion.OperationID, IfNeeded: true,
	}); !errors.Is(err, ErrAdminBootstrapAmbiguous) {
		t.Fatalf("replay error=%v, want ambiguous partial bootstrap", err)
	}
	if _, err := VerifyAudit(ctx, cfg); err == nil || !strings.Contains(err.Error(), "no matching audit event") {
		t.Fatalf("audit verification error=%v", err)
	}
}

func TestAdminBootstrapReplayRecoversPendingAuditAfterCommitCrash(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	user, err := adminauth.NewUser("admin", []byte("correct horse battery staple"), domain.AdminRoleAdministrator, now)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	originalHash := append([]byte(nil), user.PasswordHash...)
	defer clear(originalHash)
	defer clear(user.PasswordHash)
	defer clear(user.PasswordSalt)
	intent := domain.AdminAuditIntent{
		EventID: "aud_pending_bootstrap_recovery", OccurredAt: now,
		ActorType: "local_cli", ActorID: "admin", Action: "admin.bootstrap",
		TargetType: "admin_user", TargetID: "admin",
		Metadata: map[string]string{"operation_id": "install-crash-recovery", "source": "offline"},
	}
	completion := boltstore.AdminBootstrapCompletion{
		OperationID: "install-crash-recovery", Username: "admin",
		AuditEventID: intent.EventID, CommittedAt: now,
	}
	if _, _, created, err := store.CreateFirstAdminWithAuditIntent(ctx, user, completion, intent); err != nil || !created {
		store.Close()
		t.Fatalf("created=%v err=%v", created, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := BootstrapAdminWithOptions(ctx, cfg, "admin", []byte("a different correct horse battery staple"), BootstrapAdminOptions{
		OperationID: completion.OperationID, IfNeeded: true,
	})
	if err != nil || result != BootstrapAdminAlreadyCompleted {
		t.Fatalf("result=%q err=%v", result, err)
	}
	if _, err := VerifyAudit(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	store, err = boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if pending, err := store.PendingAdminAuditIntentCount(ctx); err != nil || pending != 0 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}
	persisted, err := store.GetAdminUser(ctx, "admin")
	if err != nil || !bytes.Equal(persisted.PasswordHash, originalHash) {
		t.Fatalf("retry changed administrator password: err=%v", err)
	}
}

func TestOfflineBootstrapAuditDeliveryRecoversEveryDurableBoundary(t *testing.T) {
	intent := domain.AdminAuditIntent{
		EventID: "aud_boundary_recovery", OccurredAt: time.Now().UTC(),
		ActorType: "local_cli", ActorID: "admin", Action: "admin.bootstrap",
		TargetType: "admin_user", TargetID: "admin",
		Metadata: map[string]string{"operation_id": "install-boundary-recovery", "source": "offline"},
	}
	for _, failAt := range []string{"append", "checkpoint", "delete"} {
		t.Run(failAt, func(t *testing.T) {
			pending := true
			checkpointed := false
			appended := map[string]domain.AdminAuditIntent{}
			failed := false
			deliver := func() error {
				if !pending {
					return nil
				}
				return deliverOfflineAdminAuditIntentsWith(context.Background(), []domain.AdminAuditIntent{intent},
					func(_ context.Context, current domain.AdminAuditIntent) error {
						if failAt == "append" && !failed {
							failed = true
							return errors.New("injected append failure")
						}
						if existing, ok := appended[current.EventID]; ok && !reflect.DeepEqual(existing, current) {
							return errors.New("event id payload conflict")
						}
						appended[current.EventID] = current
						return nil
					},
					func() error {
						if failAt == "checkpoint" && !failed {
							failed = true
							return errors.New("injected checkpoint failure")
						}
						checkpointed = true
						return nil
					},
					func(_ context.Context, eventID string) error {
						if failAt == "delete" && !failed {
							failed = true
							return errors.New("injected intent delete failure")
						}
						if eventID != intent.EventID {
							return errors.New("wrong intent deleted")
						}
						pending = false
						return nil
					},
				)
			}
			if err := deliver(); err == nil || !pending {
				t.Fatalf("first delivery err=%v pending=%v", err, pending)
			}
			if err := deliver(); err != nil {
				t.Fatalf("recovery: %v", err)
			}
			if pending || !checkpointed || len(appended) != 1 {
				t.Fatalf("pending=%v checkpointed=%v appended=%d", pending, checkpointed, len(appended))
			}
		})
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

	// The rate-limit window is a wall-clock minute, so an unpinned spray that
	// happens to cross a minute boundary gets a second window's allowance and
	// audits twice the records this asserts. That made the test fail about once
	// in a hundred CI runs — always looking like a regression in whatever change
	// happened to be under test. Pinning the clock is what makes the assertion
	// about the limiter rather than about how fast the machine ran.
	pinned := time.Date(2026, time.August, 6, 12, 0, 30, 0, time.UTC)
	runtime.now = func() time.Time { return pinned }

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

// The window resetting each minute is the intended design, not a leak: a
// throttled source is meant to regain its allowance, and the audit record that
// says it was throttled is meant to appear once per window rather than once.
// Stating that here keeps the ceiling above readable as "per window" — the
// number it produces across a boundary is exactly what an unpinned clock used
// to hand back as a mysterious failure.
func TestLoginRateWindowRenewsEachMinute(t *testing.T) {
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

	minute := time.Date(2026, time.August, 6, 12, 0, 30, 0, time.UTC)
	runtime.now = func() time.Time { return minute }
	before := countLoginAudits(t, runtime)

	spray := func() {
		for i := 0; i < 50; i++ {
			request := adminRequest(t, http.MethodPost, "/admin/api/v1/session/login", map[string]string{
				"username": "admin", "password": "wrong",
			})
			request.Header.Set("Origin", "https://attacker.example")
			request.RemoteAddr = "192.0.2.51:9000"
			runtime.adminRouter().ServeHTTP(httptest.NewRecorder(), request)
		}
	}
	spray()
	first := countLoginAudits(t, runtime) - before
	ceiling := cfg.Admin.LoginRPM + 1
	if first > ceiling {
		t.Fatalf("first window appended %d audit records, want at most %d", first, ceiling)
	}

	minute = minute.Add(time.Minute)
	spray()
	total := countLoginAudits(t, runtime) - before
	if total <= first {
		t.Fatal("a new minute granted no fresh allowance; the window never resets")
	}
	if total > 2*ceiling {
		t.Fatalf("two windows appended %d audit records, want at most %d", total, 2*ceiling)
	}
}

// A throttled source still has to be visible in the audit trail; the fix is to
// record it once per window, not to stop recording it.
func TestAdminRateLimiterReportsEachThrottledWindowOnce(t *testing.T) {
	var mu sync.Mutex
	windows := &adminRateState{windows: map[string]adminLoginWindow{}}
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
	// Creates require an idempotency key, so every request built here carries a
	// distinct one. A test about the key itself sets or clears its own; a test
	// about anything else should not have to know the rule exists.
	request.Header.Set("Idempotency-Key", "test-"+strconv.FormatUint(nextTestIdempotencyKey.Add(1), 10))
	return request
}

// TestAdminRateLimiterStaysBoundedUnderAddressFlood covers what the old prune
// did not. Every entry in the map belongs to the current minute, so a prune
// that deleted entries older than the minute walked the whole map on every
// request and removed nothing — quadratic work while the map still grew for as
// long as distinct addresses kept arriving, on a path reachable before
// authentication.
func TestAdminRateLimiterStaysBoundedUnderAddressFlood(t *testing.T) {
	var mu sync.Mutex
	state := &adminRateState{windows: map[string]adminLoginWindow{}}
	now := time.Date(2026, 8, 5, 9, 30, 0, 0, time.UTC)
	for attempt := range maxTrackedAdminSources * 3 {
		allowAdminRate(&mu, state, fmt.Sprintf("198.51.100.%d:40000", attempt), now, 5)
	}
	if tracked := len(state.windows); tracked > maxTrackedAdminSources {
		t.Fatalf("tracked %d sources, want at most %d", tracked, maxTrackedAdminSources)
	}
	// Rolling into a new minute hands the memory back rather than keeping the
	// array the flood grew.
	allowAdminRate(&mu, state, "203.0.113.1:1234", now.Add(time.Minute), 5)
	if tracked := len(state.windows); tracked != 1 {
		t.Fatalf("after the window rolled over, tracked=%d, want 1", tracked)
	}
}

var nextTestIdempotencyKey atomic.Uint64
