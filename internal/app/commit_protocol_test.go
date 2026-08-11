package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A mutation that is durable but not in force used to be indistinguishable from
// one that is both, because the only channel was the HTTP status. The operation
// ID is what the caller looks the change up by, and the activation header is
// what says whether it is live yet.
func TestAdminMutationReportsItsOperationAndActivationState(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	route, err := runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"public_model": route.PublicModel, "deployment_id": route.DeploymentID,
		"priority": route.Priority, "strategy": string(route.Strategy), "enabled": route.Enabled,
	}
	request := adminMutationRequest(t, http.MethodPut, "/admin/api/v1/routes/"+route.ID, session, body)
	request.Header.Set("If-Match", revisionETag(route.Revision))
	recorder := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("route update status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	operation := recorder.Header().Get("Halro-Operation-Id")
	if operation == "" {
		t.Fatal("a committed mutation did not name the operation it can be looked up by")
	}
	if activation := recorder.Header().Get("Halro-Activation"); activation != "current" {
		t.Fatalf("activation header=%q", activation)
	}
	// Delivered, so nothing is left pending and the audit log carries the very
	// event ID the caller was handed.
	pending, err := runtime.store.PendingAdminAuditIntentCount(context.Background())
	if err != nil || pending != 0 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}
	audit := authenticatedAdminGet(t, runtime, session.cookie, "/admin/api/v1/audit")
	if !bytes.Contains(audit.Body.Bytes(), []byte(operation)) {
		t.Fatalf("the audit log does not carry operation %q: %s", operation, audit.Body.String())
	}
}

// The fail-open direction F-01 and F-03 both point at: a revocation is durable,
// activation did not happen, and the old snapshot would go on authorizing. The
// data plane must refuse rather than serve from a snapshot known to be behind.
func TestStaleActivationRefusesDataPlaneTraffic(t *testing.T) {
	runtime, _, _ := activationTestRuntime(t)
	recorder := httptest.NewRecorder()
	runtime.gatewayRouter().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if recorder.Code == http.StatusServiceUnavailable {
		t.Fatalf("the runtime refused before anything was marked stale: %s", recorder.Body.String())
	}

	runtime.activation.markStale("routing registry: store unavailable", time.Now().UTC())
	recorder = httptest.NewRecorder()
	runtime.gatewayRouter().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("a stale runtime served the data plane: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var refusal struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil || refusal.Error.Code != "configuration_stale" {
		t.Fatalf("refusal does not say why: %s err=%v", recorder.Body.String(), err)
	}

	// Readiness has to agree, or an orchestrator keeps routing traffic to an
	// instance that is refusing all of it.
	ready := httptest.NewRecorder()
	runtime.gatewayRouter().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("a stale runtime reported itself ready: status=%d body=%s", ready.Code, ready.Body.String())
	}

	runtime.activation.markCurrent()
	recovered := httptest.NewRecorder()
	runtime.gatewayRouter().ServeHTTP(recovered, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if recovered.Code == http.StatusServiceUnavailable {
		t.Fatalf("the runtime kept refusing after catching up: %s", recovered.Body.String())
	}
}
