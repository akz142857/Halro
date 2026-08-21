package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
)

// reauthRequired reports whether the endpoint refused for want of step-up
// material, as opposed to succeeding or failing for any other reason. The tests
// below turn on that distinction alone, so they do not depend on a detection
// running to completion.
func reauthRequired(t *testing.T, response *httptest.ResponseRecorder) bool {
	t.Helper()
	if response.Code != http.StatusUnauthorized {
		return false
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		return false
	}
	return body.Code == "recent_reauth_required"
}

func postDetection(
	t *testing.T, runtime *Runtime, instance domain.ProviderInstance,
	session loggedInAdmin, password, key string,
) *httptest.ResponseRecorder {
	t.Helper()
	payload := map[string]any{
		"provider_model": "gpt-5.1", "target_kind": "model_id", "risk_tier": "safe_automatic",
	}
	if password != "" {
		payload["current_password"] = password
	}
	request := adminMutationRequest(t, http.MethodPost,
		"/admin/api/v1/providers/"+instance.ID+"/model-capability-detections", session, payload)
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, request)
	return response
}

// testClock is the clock these tests move, rather than reassigning Runtime.now.
//
// A detection that passes step-up is accepted and runs on its own goroutine,
// which reads the clock while the test is still going. Swapping the function
// value out from under it is a data race — one the race detector catches and
// the plain run does not, which is how it reached CI.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

func elevationRuntime(t *testing.T, window time.Duration, start time.Time) (*Runtime, domain.ProviderInstance, *testClock) {
	t.Helper()
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	value := config.Duration(window)
	runtime.config.Admin.ModelCapabilityDetection.ElevationWindow = &value
	clock := &testClock{at: start}
	// Installed once, before anything can be in flight, and never replaced.
	runtime.now = clock.now
	instance, err := runtime.store.GetProvider(t.Context(), bootstrap.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	return runtime, instance, clock
}

// The window amortises step-up over one sitting; it does not remove it. Both
// halves are pinned, because "never asks" and "always asks" are both plausible
// readings of the same code and only one is wanted.
func TestDetectionElevationWindowAsksOncePerSitting(t *testing.T) {
	start := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	runtime, instance, clock := elevationRuntime(t, 10*time.Minute, start)
	session := loginTestAdmin(t, runtime, "admin", "correct horse battery staple")

	if !reauthRequired(t, postDetection(t, runtime, instance, session, "", "one")) {
		t.Fatal("a session that never proved itself was not asked for step-up")
	}
	if reauthRequired(t, postDetection(t, runtime, instance, session, "correct horse battery staple", "two")) {
		t.Fatal("step-up with the right password was refused")
	}
	clock.set(start.Add(9 * time.Minute))
	if reauthRequired(t, postDetection(t, runtime, instance, session, "", "three")) {
		t.Fatal("an elevated session was asked again inside the window")
	}
	// Measured from the proof, not from the last use, so a long sitting asks
	// again rather than holding itself open.
	clock.set(start.Add(10*time.Minute + time.Second))
	if !reauthRequired(t, postDetection(t, runtime, instance, session, "", "four")) {
		t.Fatal("an expired elevation still started a detection")
	}
}

// The grant belongs to one session. A second session — stolen, or simply a
// second browser — inherits nothing, which is what separates this from
// remembering the answer per account.
func TestDetectionElevationIsBoundToOneSession(t *testing.T) {
	pinned := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	runtime, instance, _ := elevationRuntime(t, 10*time.Minute, pinned)
	first := loginTestAdmin(t, runtime, "admin", "correct horse battery staple")
	if reauthRequired(t, postDetection(t, runtime, instance, first, "correct horse battery staple", "one")) {
		t.Fatal("step-up was refused")
	}
	second := loginTestAdmin(t, runtime, "admin", "correct horse battery staple")
	if !reauthRequired(t, postDetection(t, runtime, instance, second, "", "two")) {
		t.Fatal("a second session inherited the first session's elevation")
	}
}

// Zero is what an operator writes to keep the prompt on every detection, and it
// has to survive Normalize, which fills absent values with the default.
func TestDetectionElevationWindowZeroAsksEveryTime(t *testing.T) {
	pinned := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	runtime, instance, _ := elevationRuntime(t, 0, pinned)
	session := loginTestAdmin(t, runtime, "admin", "correct horse battery staple")
	if reauthRequired(t, postDetection(t, runtime, instance, session, "correct horse battery staple", "one")) {
		t.Fatal("step-up was refused")
	}
	if !reauthRequired(t, postDetection(t, runtime, instance, session, "", "two")) {
		t.Fatal("a zero window still elevated the session")
	}
}

// Signing out ends the elevation with the session that earned it.
func TestDetectionElevationEndsWithTheSession(t *testing.T) {
	pinned := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	runtime, instance, _ := elevationRuntime(t, 10*time.Minute, pinned)
	session := loginTestAdmin(t, runtime, "admin", "correct horse battery staple")
	if reauthRequired(t, postDetection(t, runtime, instance, session, "correct horse battery staple", "one")) {
		t.Fatal("step-up was refused")
	}
	if got := runtime.elevationGrantCount(); got != 1 {
		t.Fatalf("expected one grant before logout, got %d", got)
	}
	logout := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/session/logout", session, map[string]any{})
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, logout)
	if response.Code >= 400 {
		t.Fatalf("logout failed: %d %s", response.Code, response.Body.String())
	}
	if got := runtime.elevationGrantCount(); got != 0 {
		t.Fatalf("logout left %d grant(s) behind", got)
	}
}

// Only detection reads the window. A delete still asks every time, because its
// damage is done the moment the request lands, where a detection's is bounded
// by MaxProviderCalls and recorded either way.
func TestDetectionElevationDoesNotCoverDeletes(t *testing.T) {
	pinned := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	runtime, instance, _ := elevationRuntime(t, 10*time.Minute, pinned)
	session := loginTestAdmin(t, runtime, "admin", "correct horse battery staple")
	if reauthRequired(t, postDetection(t, runtime, instance, session, "correct horse battery staple", "one")) {
		t.Fatal("step-up was refused")
	}
	if got := runtime.elevationGrantCount(); got != 1 {
		t.Fatalf("the session did not elevate: %d grants", got)
	}
	deleteRequest := adminMutationRequest(t, http.MethodDelete,
		"/admin/api/v1/providers/"+instance.ID, session, map[string]any{})
	// The revision precondition is checked before step-up, so without it the
	// endpoint answers 428 and the assertion below would pass without ever
	// reaching the guard it is about.
	deleteRequest.Header.Set("If-Match", fmt.Sprintf("%q", strconv.FormatUint(instance.Revision, 10)))
	response := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(response, deleteRequest)
	if !reauthRequired(t, response) {
		t.Fatalf("a delete accepted the detection elevation instead of asking: %d %s",
			response.Code, response.Body.String())
	}
}

func (r *Runtime) elevationGrantCount() int {
	r.adminElevation.mu.Lock()
	defer r.adminElevation.mu.Unlock()
	return len(r.adminElevation.grants)
}
