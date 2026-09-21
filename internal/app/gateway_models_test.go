package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/openaiapi"
)

// models.list() through the real router, with a real key and the one alias
// the bootstrap grants: the first call an SDK client makes now answers with the
// name the operator handed out, and nothing about what is behind it.
func TestModelsListsTheBootstrappedAliasThroughTheGateway(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	router := runtime.gatewayRouter()

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+bootstrap.GatewayKey)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var list openaiapi.ModelList
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" || len(list.Data) != 1 || list.Data[0].ID != "chat" || list.Data[0].OwnedBy != "halro" {
		t.Fatalf("list=%+v, want the one bootstrapped alias", list)
	}
	for _, secret := range []string{"gpt-test", "api.openai.com", bootstrap.DeploymentID} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("the model list discloses the upstream (%q): %s", secret, response.Body.String())
		}
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/models/chat", nil)
	request.Header.Set("Authorization", "Bearer "+bootstrap.GatewayKey)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("retrieve status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/models/gpt-test", nil)
	request.Header.Set("Authorization", "Bearer "+bootstrap.GatewayKey)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("the upstream model name is reachable as an alias: status=%d body=%s", response.Code, response.Body.String())
	}
}

// Inside the guarded group: an unknown key is turned away by the guard, in the
// envelope an SDK parses, and never reaches the service.
func TestModelsRefusesAnUnknownKeyBeforeTheService(t *testing.T) {
	runtime, _ := bootstrapForCapabilityTest(t)
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer gw_unknown")
	response := httptest.NewRecorder()
	runtime.gatewayRouter().ServeHTTP(response, request)
	var envelope openaiapi.ErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || response.Code != http.StatusUnauthorized || envelope.Error.Code != "invalid_api_key" {
		t.Fatalf("status=%d body=%s err=%v", response.Code, response.Body.String(), err)
	}
}

// Discovery is not exempt from the listener's staleness gate, and the manifest
// says so because it briefly said the opposite.
//
// The service deliberately skips assertPolicySnapshotsCoverProject — that check
// asks whether a Project's redaction and Token Guard policies are loaded, and
// nothing here generates anything for them to govern. It is easy to read that
// skip as "discovery answers while policy state is unsettled" and write it into
// the published contract, which is a promise about the HTTP path that the
// service level cannot make: the route sits in the same guarded group as every
// inference route, behind refuseWhileSnapshotsStale, and a durable change that
// has not reached the running snapshots refuses it identically.
//
// Fail-closed is the right answer here. The point of this test is that the
// contract must describe it rather than the reverse, and that nobody reaches for
// the outer gate to make an inaccurate deviation come true.
func TestModelsIsRefusedWhileTheSnapshotsAreStale(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	runtime.activation.markStale(activationDomainRedaction, "injected for this test", time.Now().UTC())
	if !runtime.activation.status().Stale {
		t.Fatal("the runtime did not start stale, so this test asserts nothing")
	}

	for _, path := range []string{"/v1/models", "/v1/models/chat"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+bootstrap.GatewayKey)
		response := httptest.NewRecorder()
		runtime.gatewayRouter().ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s answered %d while the snapshots were stale, want 503", path, response.Code)
		}
		if !strings.Contains(response.Body.String(), "configuration_stale") {
			t.Fatalf("%s: body=%s", path, response.Body.String())
		}
	}
}
