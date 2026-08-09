package app

import (
	"bytes"
	"context"
	"net/http"
	"testing"
)

// A deployment carries one model's own capabilities through one internal
// binding. Composing several capabilities into one outward-facing model is the
// route layer's job, so operation_bindings is refused permanently rather than
// until some later phase.
//
// The refusal is by name, not by the decoder's unknown-field rule, and that is
// the point: a request that tries this gets told what to do instead. §15 asks
// for the state to be unavailable "in an assertable way" and says explicitly
// that merely not exposing an entry point in the UI does not count.
func TestDeploymentRefusesOperationBindingsAndNamesTheAlternative(t *testing.T) {
	runtime, bootstrap := bootstrapForCapabilityTest(t)
	cookie, csrf := loginAdminForTest(t, runtime)

	body := map[string]any{
		"name": "Multi binding", "provider_id": bootstrap.ProviderID,
		"provider_model": "gpt-multi", "target_kind": "model_id",
		"mode": "operator_declared", "capabilities": map[string]any{"chat": true},
		"weight": 1,
		"operation_bindings": []map[string]any{
			{"operation": "chat", "binding_id": "b-chat"},
			{"operation": "embeddings", "binding_id": "b-embed"},
		},
	}

	current, err := runtime.store.GetDeployment(context.Background(), bootstrap.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct {
		method string
		path   string
		// The update path checks the revision precondition before it decodes,
		// so without a valid If-Match this would assert a 428 and never reach
		// the field at all.
		match string
	}{
		{http.MethodPost, "/admin/api/v1/deployments", ""},
		{http.MethodPut, "/admin/api/v1/deployments/" + bootstrap.DeploymentID, revisionETag(current.Revision)},
	} {
		response := performAdminMutation(t, runtime, cookie, csrf, call.method, call.path, call.match, body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s %s accepted operation_bindings: status=%d body=%s",
				call.method, call.path, response.Code, response.Body.String())
		}
		if !bytes.Contains(response.Body.Bytes(), []byte("operation_bindings_unavailable")) {
			t.Fatalf("%s %s refused without a stable code: %s", call.method, call.path, response.Body.String())
		}
		// The refusal has to point at the route layer. Without that an operator
		// is left with "no" and no way to build what they were asking for.
		for _, expected := range []string{"one deployment per internal binding", "route", "public model"} {
			if !bytes.Contains(response.Body.Bytes(), []byte(expected)) {
				t.Fatalf("%s %s refusal does not say what to do instead (missing %q): %s",
					call.method, call.path, expected, response.Body.String())
			}
		}
	}

	// Nothing was written on the way to being refused.
	listed := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/deployments")
	if bytes.Contains(listed.Body.Bytes(), []byte("gpt-multi")) {
		t.Fatalf("a rejected multi-binding deployment was persisted: %s", listed.Body.String())
	}
	if bytes.Contains(listed.Body.Bytes(), []byte("operation_bindings")) {
		t.Fatalf("deployments carry an operation_bindings field: %s", listed.Body.String())
	}

	// An explicit null says nothing, the same as omitting the field, and must not
	// be read as an attempt — otherwise a client that serialises every field
	// cannot create a deployment at all.
	permitted := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/deployments", "", map[string]any{
		"name": "Single binding", "provider_id": bootstrap.ProviderID,
		"provider_model": "gpt-single", "target_kind": "model_id",
		"mode": "operator_declared", "capabilities": map[string]any{"chat": true},
		"weight": 1, "operation_bindings": nil,
	})
	if permitted.Code != http.StatusCreated {
		t.Fatalf("an explicit null was treated as an attempt: status=%d body=%s", permitted.Code, permitted.Body.String())
	}
}
