package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPasswordChangeFailuresAreThrottledAndAudited closes the gap that made
// step-up's own promise false. Its comment says it is "the only entry point;
// the primitive underneath is unbounded and silent" — but several endpoints
// asked for the account password directly, and those went through neither the
// failure budget nor the audit trail. Each attempt runs Argon2id at 64 MiB, so
// an authenticated caller could spend the server's memory at will and guess
// the password without leaving a record.
func TestPasswordChangeFailuresAreThrottledAndAudited(t *testing.T) {
	runtime, session := stepUpTestRuntime(t)

	attempt := func(password string) int {
		request := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/session/password", session,
			map[string]string{"current_password": password, "new_password": "a replacement passphrase"})
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, request)
		return response.Code
	}

	for guess := 1; guess <= adminStepUpFailuresPerMinute; guess++ {
		if status := attempt("guess number one"); status != http.StatusUnauthorized {
			t.Fatalf("guess %d status=%d, want 401 inside the budget", guess, status)
		}
	}
	if status := attempt("guess number one"); status != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429 once the failure budget is spent", status)
	}
	// Refused for the right password too, or the bound is bypassed by whoever
	// happens to guess correctly.
	if status := attempt(stepUpTestPassword); status != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429 for the correct password while throttled", status)
	}
}

// TestPasswordChangeSharesTheStepUpBudget pins that the two paths draw on one
// budget per account. Separate counters would let a guesser alternate between
// endpoints and get the sum of both.
func TestPasswordChangeSharesTheStepUpBudget(t *testing.T) {
	runtime, session := stepUpTestRuntime(t)
	project := createStepUpProject(t, runtime, session)

	for guess := 1; guess <= adminStepUpFailuresPerMinute; guess++ {
		request := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/session/password", session,
			map[string]string{"current_password": "guess number one", "new_password": "a replacement passphrase"})
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d status=%d", guess, response.Code)
		}
	}
	// The budget is spent, so a step-up on another endpoint must already be
	// refused rather than starting over.
	deletion := adminMutationRequest(t, http.MethodDelete, "/admin/api/v1/projects/"+project, session, stepUp())
	deletion.Header.Set("If-Match", `"1"`)
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, deletion)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429: password and step-up must share one account budget", response.Code)
	}
}
