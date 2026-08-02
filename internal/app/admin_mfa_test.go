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

	"github.com/akz142857/Heimdall/internal/adminauth"
	"github.com/akz142857/Heimdall/internal/domain"
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
