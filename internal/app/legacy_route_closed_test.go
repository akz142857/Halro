package app

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

// The old shape reached a provider directly, which meant no versioned price, no
// health probe, no capability snapshot and no deployment concurrency limit —
// four governed things, silently absent. The Admin API must refuse it rather
// than accept a route that quietly opts out of all of them.
func TestAdminRefusesARouteWithoutADeployment(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	cookie, csrf := loginAdminForTest(t, runtime)

	// The old field names are gone from the input struct, and decodeAdminJSON
	// rejects unknown fields, so an old client gets a 400 rather than a route
	// with its provider silently dropped.
	legacy := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
		"/admin/api/v1/routes", "", map[string]any{
			"public_model": "legacy-shape", "provider_id": bootstrap.ProviderID,
			"provider_model": "gpt-test", "enabled": false,
		})
	if legacy.Code != http.StatusBadRequest {
		t.Fatalf("the old route shape was not refused: status=%d body=%s", legacy.Code, legacy.Body.String())
	}

	// Omitting the deployment entirely is refused too, by validation rather
	// than by the unknown-field check.
	empty := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
		"/admin/api/v1/routes", "", map[string]any{"public_model": "no-deployment", "enabled": false})
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("a route with no deployment was accepted: status=%d body=%s", empty.Code, empty.Body.String())
	}
	if !bytes.Contains(empty.Body.Bytes(), []byte("deployment id is required")) {
		t.Fatalf("refusal does not name the missing field: %s", empty.Body.String())
	}

	// The supported shape still works.
	ok := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
		"/admin/api/v1/routes", "", map[string]any{
			"public_model": "supported", "deployment_id": bootstrap.DeploymentID, "enabled": false,
		})
	if ok.Code != http.StatusCreated {
		t.Fatalf("a deployment-backed route was refused: status=%d body=%s", ok.Code, ok.Body.String())
	}
}

// The store is the last line: even a caller bypassing the handler cannot
// persist the old shape.
func TestStoreRefusesARouteWithoutADeployment(t *testing.T) {
	runtime, _ := bootstrapForCapabilityTest(t)

	_, err := runtime.store.PutRoute(context.Background(), domain.Route{
		ID: "rte-orphan", PublicModel: "orphan", Enabled: true,
	}, 0)
	if err == nil {
		t.Fatal("the store persisted a route with no deployment")
	}
}
