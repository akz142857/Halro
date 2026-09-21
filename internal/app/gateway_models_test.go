package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
