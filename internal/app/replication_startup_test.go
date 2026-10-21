package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/replication"
)

func TestPrimaryRefusesTrafficUntilStartupPeersAreAdjudicated(t *testing.T) {
	runtime := &Runtime{replication: &replicationRuntime{role: replication.RolePrimary}}
	served := false
	handler := runtime.requireMemberStartup(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		served = true
		writer.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || served {
		t.Fatalf("startup-gated response=%d served=%v, want 503 and no downstream call", response.Code, served)
	}
	if response.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After=%q, want 1", response.Header().Get("Retry-After"))
	}

	runtime.replication.startupReady.Store(true)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || !served {
		t.Fatalf("adjudicated response=%d served=%v, want downstream 204", response.Code, served)
	}
}

func TestPrimaryStartupGateLeavesHealthAndClusterStatusObservable(t *testing.T) {
	runtime := &Runtime{replication: &replicationRuntime{role: replication.RolePrimary}}
	handler := runtime.requireMemberStartup(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	for _, path := range []string{"/health/live", "/health/ready", "/admin/api/v1/cluster/status"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNoContent {
			t.Fatalf("path %s response=%d, want observable downstream response", path, response.Code)
		}
	}
}

func TestPrimaryReadinessReportsPendingStartupAdjudication(t *testing.T) {
	runtime := &Runtime{replication: &replicationRuntime{role: replication.RolePrimary}}
	response := httptest.NewRecorder()
	runtime.ready(response, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("response=%d, want 503", response.Code)
	}
	if body := response.Body.String(); body == "" || !containsAll(body, `"status":"not_ready"`, `"replication":"awaiting_peer_adjudication"`) {
		t.Fatalf("unexpected readiness body %s", body)
	}
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}
