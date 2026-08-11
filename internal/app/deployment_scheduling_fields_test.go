package app

import (
	"context"
	"net/http"
	"testing"
)

// priority and weight were stored on a Deployment, validated, and echoed back by
// the API, while the router read neither: candidate ordering comes from the
// Route's priority, and round-robin rotates candidates without consulting any
// weight — provider.Target has no weight field to consult. Editing them changed
// nothing about how requests were distributed.
//
// Pre-1.0 the wrong construct goes rather than gaining a corrected sibling, so
// the fields are gone. The decoder's unknown-field rule then makes the removal
// audible: a client still sending them is told, instead of having them silently
// dropped and being left believing it configured something.
func TestDeploymentRejectsTheRemovedSchedulingFields(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	cookie, csrf := loginAdminForTest(t, runtime)
	current, err := runtime.store.GetDeployment(context.Background(), bootstrap.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}

	for _, field := range []string{"priority", "weight"} {
		body := map[string]any{
			"name": "Scheduling", "provider_id": bootstrap.ProviderID,
			"provider_model": "gpt-test", "target_kind": "model_id",
			"mode": "operator_declared", "capabilities": map[string]any{"chat": true},
			field: 3,
		}
		for _, call := range []struct{ method, path, match string }{
			{http.MethodPost, "/admin/api/v1/deployments", ""},
			{http.MethodPut, "/admin/api/v1/deployments/" + bootstrap.DeploymentID, revisionETag(current.Revision)},
		} {
			response := performAdminMutation(t, runtime, cookie, csrf, call.method, call.path, call.match, body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("%s %s accepted %q: status=%d body=%s",
					call.method, call.path, field, response.Code, response.Body.String())
			}
		}
	}
}

// The Route keeps its priority, because that is the one the router actually
// reads. Removing the Deployment's must not have taken it with it.
func TestRoutePriorityStillOrdersCandidates(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	route, err := runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	route.Priority = 7
	if _, err := runtime.store.PutRoute(context.Background(), route, route.Revision); err != nil {
		t.Fatal(err)
	}
	if err := runtime.activateTopology(); err != nil {
		t.Fatal(err)
	}
	targets := runtime.providers.ResolveAll(route.PublicModel)
	if len(targets) != 1 {
		t.Fatalf("targets=%+v", targets)
	}
	if targets[0].Priority != 7 {
		t.Fatalf("route priority did not reach the routing target: %d", targets[0].Priority)
	}
}
