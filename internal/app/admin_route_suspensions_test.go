package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/routegate"
)

// The one surface that names identity.
//
// The caller-facing 503 names nothing and the metrics carry enumerations only,
// both deliberately — so an operator answering HalroCredentialUnusable has
// exactly one place to learn which credential, and it has to actually say.
func TestRouteSuspensionsNameTheScopeAnOperatorHasToFix(t *testing.T) {
	gate := routegate.New(routegate.Config{})
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	gate.Observe(provider.Target{
		ID: "route_1", DeploymentID: "dep_1", ProviderID: "provider_1",
		ProviderModel: "gpt-test", CredentialID: "cred_live", CredentialRevision: 7,
	}, routegate.Observation{
		Reason: provider.FailureReasonInvalidCredential, Status: 401, Code: "invalid_api_key",
	}, now)
	runtime := &Runtime{routes: gate, now: func() time.Time { return now }}

	recorder := httptest.NewRecorder()
	runtime.listAdminRouteSuspensions(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Items []routeSuspensionView `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items=%+v", payload.Items)
	}
	item := payload.Items[0]
	if item.ScopeKind != "credential" || item.ScopeKey != "cred_live" {
		t.Fatalf("scope=%s/%s, want credential/cred_live", item.ScopeKind, item.ScopeKey)
	}
	if !item.Indefinite || item.Until != "" {
		t.Fatalf("a dead credential was given an end time: %+v", item)
	}
	// The revision is what the operator is comparing against when they wonder
	// why the suspension is still there after saving a new secret.
	if item.CredentialRevision != 7 {
		t.Fatalf("credential_revision=%d, want 7", item.CredentialRevision)
	}
	if item.Reason != "invalid_credential" || item.Status != 401 || item.Code != "invalid_api_key" {
		t.Fatalf("evidence lost: %+v", item)
	}
}

// The credential-and-model scope joins its halves with a NUL so they cannot
// collide inside the gate. That byte has no business in a JSON body an operator
// reads, or in anything that pastes it into a search box.
func TestRouteSuspensionKeysAreLegibleOutsideTheGate(t *testing.T) {
	gate := routegate.New(routegate.Config{})
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	gate.Observe(provider.Target{
		ID: "route_1", DeploymentID: "dep_1", ProviderID: "provider_1",
		ProviderModel: "gpt-test", CredentialID: "cred_live", CredentialRevision: 1,
	}, routegate.Observation{
		Reason: provider.FailureReasonSubscriptionQuotaExhausted, Status: 402,
	}, now)
	runtime := &Runtime{routes: gate, now: func() time.Time { return now }}

	recorder := httptest.NewRecorder()
	runtime.listAdminRouteSuspensions(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	body := recorder.Body.String()
	if strings.ContainsRune(body, 0) {
		t.Fatalf("a NUL reached the response body: %q", body)
	}
	if !strings.Contains(body, "cred_live/gpt-test") {
		t.Fatalf("the credential-and-model scope did not render legibly: %s", body)
	}
	if !strings.Contains(body, `"until"`) {
		t.Fatalf("a suspension that ends on a clock did not say when: %s", body)
	}
}
