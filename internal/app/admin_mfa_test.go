package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/adminauth"
	"github.com/akz142857/Halro/internal/domain"
)

func TestAdminMFALoginRequiresAndConsumesSecondFactor(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	password := "correct horse battery staple"
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte(password)); err != nil {
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
	if _, err = runtime.store.PutAdminUser(context.Background(), user, user.Revision); err != nil {
		t.Fatal(err)
	}
	secret := []byte("12345678901234567890")
	ciphertext, err := runtime.vault.EncryptAdminMFA("mfa_test", "admin", secret)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	authenticator := domain.AdminMFAAuthenticator{ID: "mfa_test", Username: "admin", Name: "test phone", Type: domain.AdminMFATypeTOTP, SecretCiphertext: ciphertext, Status: domain.AdminMFAStatusActive, CreatedAt: now, ConfirmedAt: &now}
	if _, err = runtime.store.PutAdminMFAAuthenticator(context.Background(), authenticator, 0); err != nil {
		t.Fatal(err)
	}
	login := adminRequest(t, http.MethodPost, "/admin/api/v1/session/login", map[string]string{"username": "admin", "password": password})
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, login)
	if response.Code != http.StatusAccepted || len(response.Result().Cookies()) != 0 {
		t.Fatalf("password login status=%d cookies=%v body=%s", response.Code, response.Result().Cookies(), response.Body.String())
	}
	var challenge struct {
		ChallengeToken string `json:"challenge_token"`
	}
	if json.Unmarshal(response.Body.Bytes(), &challenge) != nil || challenge.ChallengeToken == "" {
		t.Fatal("missing challenge")
	}
	code := adminauth.TOTPCode(secret, time.Now().Unix()/adminauth.TOTPPeriod)
	complete := adminRequest(t, http.MethodPost, "/admin/api/v1/session/mfa/totp", map[string]string{"challenge_token": challenge.ChallengeToken, "code": code})
	completeResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(completeResponse, complete)
	if completeResponse.Code != http.StatusOK || len(completeResponse.Result().Cookies()) != 1 {
		t.Fatalf("MFA status=%d body=%s", completeResponse.Code, completeResponse.Body.String())
	}
	var completedSession struct {
		Appearance string `json:"appearance"`
	}
	if err = json.Unmarshal(completeResponse.Body.Bytes(), &completedSession); err != nil || completedSession.Appearance != domain.AppearanceLight {
		t.Fatalf("MFA session appearance=%q error=%v body=%s", completedSession.Appearance, err, completeResponse.Body.String())
	}
	replay := adminRequest(t, http.MethodPost, "/admin/api/v1/session/mfa/totp", map[string]string{"challenge_token": challenge.ChallengeToken, "code": code})
	replayResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(replayResponse, replay)
	if replayResponse.Code != http.StatusUnauthorized {
		t.Fatalf("challenge replay status=%d", replayResponse.Code)
	}
}

func TestRequiredMFAPolicyRestrictsUnenrolledSessionToSetup(t *testing.T) {
	cfg := testConfig(t)
	cfg.Admin.MFAPolicy = "required"
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	password := "correct horse battery staple"
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte(password)); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	login := adminRequest(t, http.MethodPost, "/admin/api/v1/session/login", map[string]string{"username": "admin", "password": password})
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, login)
	if response.Code != http.StatusOK {
		t.Fatalf("login status=%d", response.Code)
	}
	cookie := response.Result().Cookies()[0]
	dashboard := adminRequest(t, http.MethodGet, "/admin/api/v1/dashboard", nil)
	dashboard.AddCookie(cookie)
	dashboardResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(dashboardResponse, dashboard)
	if dashboardResponse.Code != http.StatusForbidden {
		t.Fatalf("unenrolled dashboard status=%d", dashboardResponse.Code)
	}
	status := adminRequest(t, http.MethodGet, "/admin/api/v1/security/mfa", nil)
	status.AddCookie(cookie)
	statusResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(statusResponse, status)
	if statusResponse.Code != http.StatusOK {
		t.Fatalf("MFA setup status=%d", statusResponse.Code)
	}
	var loginBody struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &loginBody); err != nil {
		t.Fatal(err)
	}
	logout := adminRequest(t, http.MethodPost, "/admin/api/v1/session/logout", nil)
	logout.AddCookie(cookie)
	logout.Header.Set("X-CSRF-Token", loginBody.CSRFToken)
	logoutResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(logoutResponse, logout)
	if logoutResponse.Code != http.StatusOK {
		t.Fatalf("unenrolled logout status=%d body=%s", logoutResponse.Code, logoutResponse.Body.String())
	}
}

func TestAdministratorsRequiredMFAPolicyExemptsReadOnlyAccounts(t *testing.T) {
	cfg := testConfig(t)
	cfg.Admin.MFAPolicy = "administrators_required"
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	adminPassword := "correct horse battery staple"
	readerPassword := "another correct horse battery staple"
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte(adminPassword)); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	reader, err := adminauth.NewUser("reader", []byte(readerPassword), domain.AdminRoleReadOnly, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	defer clear(reader.PasswordHash)
	defer clear(reader.PasswordSalt)
	if _, err = runtime.store.PutAdminUser(context.Background(), reader, 0); err != nil {
		t.Fatal(err)
	}

	var readerSession loggedInAdmin
	for _, test := range []struct {
		username string
		password string
		required bool
		status   int
	}{
		{"admin", adminPassword, true, http.StatusForbidden},
		{"reader", readerPassword, false, http.StatusOK},
	} {
		t.Run(test.username, func(t *testing.T) {
			session := loginTestAdmin(t, runtime, test.username, test.password)
			if test.username == "reader" {
				readerSession = session
			}
			current := authenticatedAdminGet(t, runtime, session.cookie, "/admin/api/v1/session")
			if current.Code != http.StatusOK {
				t.Fatalf("session status=%d body=%s", current.Code, current.Body.String())
			}
			var sessionBody struct {
				MFASetupRequired bool `json:"mfa_setup_required"`
			}
			if err := json.Unmarshal(current.Body.Bytes(), &sessionBody); err != nil {
				t.Fatal(err)
			}
			if sessionBody.MFASetupRequired != test.required {
				t.Errorf("mfa_setup_required=%t, want %t", sessionBody.MFASetupRequired, test.required)
			}

			mfa := authenticatedAdminGet(t, runtime, session.cookie, "/admin/api/v1/security/mfa")
			if mfa.Code != http.StatusOK {
				t.Fatalf("MFA status=%d body=%s", mfa.Code, mfa.Body.String())
			}
			var mfaBody struct {
				Policy   string `json:"policy"`
				Required bool   `json:"required"`
			}
			if err := json.Unmarshal(mfa.Body.Bytes(), &mfaBody); err != nil {
				t.Fatal(err)
			}
			if mfaBody.Policy != "administrators_required" || mfaBody.Required != test.required {
				t.Errorf("MFA policy=%q required=%t, want administrators_required/%t", mfaBody.Policy, mfaBody.Required, test.required)
			}

			dashboard := authenticatedAdminGet(t, runtime, session.cookie, "/admin/api/v1/dashboard")
			if dashboard.Code != test.status {
				t.Errorf("dashboard status=%d, want %d; body=%s", dashboard.Code, test.status, dashboard.Body.String())
			}
		})
	}

	// A read-only account may still enroll voluntarily. The role-scoped policy
	// must let that account remove its own factor again; otherwise the login gate
	// would be optional while enrollment remained irreversible.
	secret := []byte("12345678901234567890")
	ciphertext, err := runtime.vault.EncryptAdminMFA("mfa_reader", "reader", secret)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err = runtime.store.PutAdminMFAAuthenticator(context.Background(), domain.AdminMFAAuthenticator{
		ID: "mfa_reader", Username: "reader", Name: "reader phone", Type: domain.AdminMFATypeTOTP,
		SecretCiphertext: ciphertext, Status: domain.AdminMFAStatusActive, CreatedAt: now, ConfirmedAt: &now,
	}, 0); err != nil {
		t.Fatal(err)
	}
	disable := performAdminMutation(t, runtime, readerSession.cookie, readerSession.csrf, http.MethodDelete,
		"/admin/api/v1/security/mfa", "", map[string]string{
			"current_password": readerPassword,
			"code":             adminauth.TOTPCode(secret, time.Now().Unix()/adminauth.TOTPPeriod),
		})
	if disable.Code != http.StatusOK {
		t.Fatalf("read_only MFA disable status=%d body=%s", disable.Code, disable.Body.String())
	}
}
