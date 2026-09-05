package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/adminauth"
	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/domain"
)

// TestFailedMFACodesAreAuditedAndBounded covers the second factor's blind
// spot. Password failures have been audited from the beginning; the code that
// guards the account once the password is already known left no record at all,
// so the one credential worth guessing was the one guessing could be done
// against silently.
//
// The bound itself turned out to already exist, in a place worth pinning:
// PutAdminMFAChallenge carries AttemptsRemaining forward when it re-issues a
// challenge for the same user and session generation, so minting a fresh
// challenge per guess does not buy fresh attempts. This covers both halves —
// the record that was missing, and the carry-forward that was load-bearing
// without being tested.
func TestFailedMFACodesAreAuditedAndBounded(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	const password = "correct horse battery staple"
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
	if _, err = runtime.store.PutAdminMFAAuthenticator(context.Background(), domain.AdminMFAAuthenticator{
		ID: "mfa_test", Username: "admin", Name: "test phone", Type: domain.AdminMFATypeTOTP,
		SecretCiphertext: ciphertext, Status: domain.AdminMFAStatusActive, CreatedAt: now, ConfirmedAt: &now,
	}, 0); err != nil {
		t.Fatal(err)
	}

	// A fresh challenge and a fresh source address per guess, which is what a
	// distributed guesser would do: it defeats allowAdminLogin, the only limit
	// on this path that was visible from the handler.
	attemptNumber := 0
	guess := func(code string) int {
		attemptNumber++
		source := fmt.Sprintf("198.51.100.%d:40000", attemptNumber)
		login := adminRequest(t, http.MethodPost, "/admin/api/v1/session/login",
			map[string]string{"username": "admin", "password": password})
		login.RemoteAddr = source
		loginResponse := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(loginResponse, login)
		if loginResponse.Code != http.StatusAccepted {
			t.Fatalf("password login status=%d body=%s", loginResponse.Code, loginResponse.Body.String())
		}
		var challenge struct {
			ChallengeToken string `json:"challenge_token"`
		}
		if json.Unmarshal(loginResponse.Body.Bytes(), &challenge) != nil || challenge.ChallengeToken == "" {
			t.Fatal("missing challenge")
		}
		complete := adminRequest(t, http.MethodPost, "/admin/api/v1/session/mfa/totp",
			map[string]string{"challenge_token": challenge.ChallengeToken, "code": code})
		complete.RemoteAddr = source
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, complete)
		return response.Code
	}

	const budget = 5
	for attempt := 1; attempt <= budget; attempt++ {
		if status := guess("000000"); status != http.StatusUnauthorized {
			t.Fatalf("guess %d status=%d, want 401", attempt, status)
		}
	}
	// The sixth guess is refused before any code is checked: re-issuing the
	// challenge did not restore the spent attempts.
	if status := guess("000000"); status != http.StatusUnauthorized {
		t.Fatalf("status=%d, want the exhausted challenge budget to keep refusing", status)
	}

	failures := 0
	if _, err := runtime.audit.Replay(func(record audit.Record) error {
		if record.Event.Action == "admin.reauthentication" && record.Event.Outcome == "failure" {
			failures++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if failures != budget {
		t.Fatalf("audited MFA failures=%d, want %d: guessing the second factor must leave a record", failures, budget)
	}
}

func TestAdminMFAManagementFactorFailuresShareTheCredentialBudget(t *testing.T) {
	endpoints := []struct {
		name   string
		method string
		path   string
		body   map[string]string
	}{
		{"create authenticator", http.MethodPost, "/admin/api/v1/security/mfa/authenticators", map[string]string{"name": "extra"}},
		{"regenerate recovery codes", http.MethodPost, "/admin/api/v1/security/mfa/recovery-codes/regenerate", nil},
		{"disable MFA", http.MethodDelete, "/admin/api/v1/security/mfa", nil},
		{"delete authenticator", http.MethodDelete, "/admin/api/v1/security/mfa/authenticators/mfa_budget", nil},
	}
	for _, endpoint := range endpoints {
		t.Run(endpoint.name, func(t *testing.T) {
			runtime, session := stepUpTestRuntime(t)
			secret := []byte("12345678901234567890")
			ciphertext, err := runtime.vault.EncryptAdminMFA("mfa_budget", "admin", secret)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			if _, err = runtime.store.PutAdminMFAAuthenticator(context.Background(), domain.AdminMFAAuthenticator{
				ID: "mfa_budget", Username: "admin", Name: "budget", Type: domain.AdminMFATypeTOTP,
				SecretCiphertext: ciphertext, Status: domain.AdminMFAStatusActive, CreatedAt: now, ConfirmedAt: &now,
			}, 0); err != nil {
				t.Fatal(err)
			}
			wrongCode := "000000"
			for offset := int64(-1); offset <= 1; offset++ {
				if wrongCode == adminauth.TOTPCode(secret, time.Now().Unix()/adminauth.TOTPPeriod+offset) {
					wrongCode = "111111"
					break
				}
			}
			for attempt := 0; attempt < adminStepUpFailuresPerMinute+1; attempt++ {
				body := map[string]string{"current_password": stepUpTestPassword, "code": wrongCode}
				for key, value := range endpoint.body {
					body[key] = value
				}
				request := adminMutationRequest(t, endpoint.method, endpoint.path, session, body)
				response := httptest.NewRecorder()
				runtime.adminRouter().ServeHTTP(response, request)
				want := http.StatusUnauthorized
				if attempt == adminStepUpFailuresPerMinute {
					want = http.StatusTooManyRequests
				}
				if response.Code != want {
					t.Fatalf("attempt %d status=%d, want %d body=%s", attempt+1, response.Code, want, response.Body.String())
				}
			}
			failures := 0
			if _, err := runtime.audit.Replay(func(record audit.Record) error {
				if record.Event.Action == "admin.reauthentication" && record.Event.Outcome == "failure" {
					failures++
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if failures != adminStepUpFailuresPerMinute {
				t.Fatalf("audited failures=%d, want %d", failures, adminStepUpFailuresPerMinute)
			}
		})
	}
}
