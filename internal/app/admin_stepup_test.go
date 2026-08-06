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

// stepUp is the re-authentication body every destructive Admin DELETE now
// carries. The password matches what these tests bootstrap the admin with.
func stepUp() map[string]string {
	return map[string]string{"current_password": "correct horse battery staple"}
}

const stepUpTestPassword = "correct horse battery staple"

func stepUpTestRuntime(t *testing.T) (*Runtime, loggedInAdmin) {
	t.Helper()
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte(stepUpTestPassword)); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	return runtime, loginTestAdmin(t, runtime, "admin", stepUpTestPassword)
}

// Swept rather than listed: a delete endpoint added later is in scope the day
// it is registered, without anyone remembering to extend a checklist. The
// deletes that carry their re-authentication inline are named because they
// enforce the same rule through their own code, not because they are exempt.
func TestEveryDestructiveDeleteRequiresStepUp(t *testing.T) {
	runtime, session := stepUpTestRuntime(t)

	inlineReauth := map[string]bool{
		// Verify password + TOTP inside the handler itself.
		"DELETE /admin/api/v1/security/mfa":                   true,
		"DELETE /admin/api/v1/security/mfa/authenticators/{}": true,
		"DELETE /admin/api/v1/admin-users/{}":                 true,
		// Not destructive: these withdraw something the caller just started.
		"DELETE /admin/api/v1/session/mfa/challenge":                  true,
		"DELETE /admin/api/v1/security/mfa/authenticators/{}/pending": true,
		"DELETE /admin/api/v1/settings/accounting/pending":            true,
	}
	parameter := regexp.MustCompile(`\{[^}]+\}`)
	routes, ok := runtime.adminRouter().(chi.Routes)
	if !ok {
		t.Fatal("admin router does not expose chi route metadata")
	}
	swept := 0
	if err := chi.Walk(routes, func(method, path string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method != http.MethodDelete || !strings.HasPrefix(path, "/admin/api/") {
			return nil
		}
		if inlineReauth[method+" "+parameter.ReplaceAllString(path, "{}")] {
			return nil
		}
		swept++
		concretePath := parameter.ReplaceAllString(path, "placeholder")
		// A revision is supplied so the request gets past the precondition and
		// the step-up is genuinely the thing that stops it.
		request := adminMutationRequest(t, method, concretePath, session, nil)
		request.Header.Set("If-Match", `"1"`)
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: no step-up material, status=%d body=%s", method, path, response.Code, response.Body.String())
			return nil
		}
		var body struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Code != "recent_reauth_required" {
			t.Errorf("%s %s: 401 was not the step-up gate (code=%q)", method, path, body.Code)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if swept < 9 {
		t.Fatalf("only swept %d destructive deletes — the walk likely missed most of adminRouter()", swept)
	}
}

func TestStepUpRejectsAWrongPasswordAndAcceptsTheRightOne(t *testing.T) {
	runtime, session := stepUpTestRuntime(t)
	project := createStepUpProject(t, runtime, session)

	wrong := adminMutationRequest(t, http.MethodDelete, "/admin/api/v1/projects/"+project, session,
		map[string]string{"current_password": "not the admin password"})
	wrong.Header.Set("If-Match", `"1"`)
	wrongResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(wrongResponse, wrong)
	if wrongResponse.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status=%d body=%s", wrongResponse.Code, wrongResponse.Body.String())
	}

	right := adminMutationRequest(t, http.MethodDelete, "/admin/api/v1/projects/"+project, session, stepUp())
	right.Header.Set("If-Match", `"1"`)
	rightResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(rightResponse, right)
	if rightResponse.Code != http.StatusNoContent {
		t.Fatalf("correct password status=%d body=%s", rightResponse.Code, rightResponse.Body.String())
	}
}

// An authenticated session is otherwise an offline-speed password oracle: the
// caller already holds a valid cookie, so without this nothing slows a guess.
func TestRepeatedStepUpFailuresAreThrottledPerAccount(t *testing.T) {
	runtime, session := stepUpTestRuntime(t)
	project := createStepUpProject(t, runtime, session)

	attempt := func(password string) int {
		request := adminMutationRequest(t, http.MethodDelete, "/admin/api/v1/projects/"+project, session,
			map[string]string{"current_password": password})
		request.Header.Set("If-Match", `"1"`)
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
	// The correct password is refused too while the account is throttled —
	// otherwise the limit is bypassed by whoever happens to guess right.
	if status := attempt(stepUpTestPassword); status != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429 for the correct password while throttled", status)
	}
}

// Successful step-ups must not consume the budget, or an operator cleaning up
// six resources is locked out halfway through for doing nothing wrong.
func TestSuccessfulStepUpsDoNotConsumeTheFailureBudget(t *testing.T) {
	runtime, session := stepUpTestRuntime(t)
	for round := 0; round < adminStepUpFailuresPerMinute+3; round++ {
		project := createStepUpProject(t, runtime, session)
		request := adminMutationRequest(t, http.MethodDelete, "/admin/api/v1/projects/"+project, session, stepUp())
		request.Header.Set("If-Match", `"1"`)
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("delete %d status=%d body=%s", round, response.Code, response.Body.String())
		}
	}
}

func createStepUpProject(t *testing.T, runtime *Runtime, session loggedInAdmin) string {
	t.Helper()
	create := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/projects", session, map[string]any{
		"name": "project-" + t.Name() + "-" + randomProjectSuffix(t),
	})
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, create)
	if response.Code != http.StatusCreated {
		t.Fatalf("create project status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

var projectSuffix int

func randomProjectSuffix(t *testing.T) string {
	t.Helper()
	projectSuffix++
	return string(rune('a' + projectSuffix%26))
}
