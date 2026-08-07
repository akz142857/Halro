package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMintingAGatewayKeyRequiresStepUp is the criterion correction. Step-up was
// scoped to deletions, which left the single most valuable thing a stolen
// session could do wide open: a Gateway Key is returned in plaintext once,
// stays valid after the Admin session that minted it is revoked, and spends the
// project's budget. That is a strictly better outcome for a thief than any of
// the deletions the gate already covered.
func TestMintingAGatewayKeyRequiresStepUp(t *testing.T) {
	runtime, session := stepUpTestRuntime(t)
	project := createStepUpProject(t, runtime, session)

	mint := func(body map[string]string) *httptest.ResponseRecorder {
		request := adminMutationRequest(t, http.MethodPost,
			"/admin/api/v1/projects/"+project+"/keys", session, body)
		request.Header.Set("Idempotency-Key", "idem-"+body["name"])
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, request)
		return response
	}

	if status := mint(map[string]string{"name": "no-proof"}).Code; status != http.StatusUnauthorized {
		t.Fatalf("minting without step-up status=%d, want 401", status)
	}
	if status := mint(map[string]string{"name": "wrong", "current_password": "not the admin password"}).Code; status != http.StatusUnauthorized {
		t.Fatalf("minting with a wrong password status=%d, want 401", status)
	}
	response := mint(map[string]string{"name": "proven", "current_password": stepUpTestPassword})
	if response.Code != http.StatusCreated {
		t.Fatalf("minting with the correct password status=%d body=%s", response.Code, response.Body.String())
	}
	// The plaintext still comes back exactly once, so the gate did not cost the
	// property the whole flow exists for.
	var created struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(response.Body.Bytes(), &created) != nil || created.Key == "" {
		t.Fatalf("expected a one-time plaintext key, body=%s", response.Body.String())
	}
}

// TestUnblockingAProjectRequiresStepUp covers the other half of the criterion:
// not only what destroys state, but what switches off a protection currently in
// force. Token Guard blocked a caller for a reason; lifting that is a security
// decision, and a stolen session should not be able to make it alone.
func TestUnblockingAProjectRequiresStepUp(t *testing.T) {
	runtime, session := stepUpTestRuntime(t)
	project := createStepUpProject(t, runtime, session)

	unblock := func(body map[string]string) int {
		request := adminMutationRequest(t, http.MethodPost,
			"/admin/api/v1/projects/"+project+"/unblock", session, body)
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, request)
		return response.Code
	}

	if status := unblock(map[string]string{}); status != http.StatusUnauthorized {
		t.Fatalf("unblocking without step-up status=%d, want 401", status)
	}
	if status := unblock(map[string]string{"current_password": stepUpTestPassword}); status != http.StatusOK {
		t.Fatalf("unblocking with the correct password status=%d, want 200", status)
	}
}
