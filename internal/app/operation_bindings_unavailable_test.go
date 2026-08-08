package app

import (
	"bytes"
	"context"
	"net/http"
	"testing"
)

// §15: multi-binding deployments must stay unavailable "in an assertable way"
// until Phase 3 lands — a request carrying operation_bindings has to be
// refused, and the gate says explicitly that merely not exposing an entry point
// in the UI does not count.
//
// The refusal comes from decodeAdminJSON rejecting unknown fields rather than
// from a named check, which is the right shape: adding the field to the input
// struct is what should make it reachable, so the day it appears this test is
// what says the runtime gates have to come with it.
func TestDeploymentRejectsOperationBindingsUntilPhase3(t *testing.T) {
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
		if !bytes.Contains(response.Body.Bytes(), []byte("invalid request")) {
			t.Fatalf("%s %s refusal=%s", call.method, call.path, response.Body.String())
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
}
