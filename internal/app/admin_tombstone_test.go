package app

// A tombstone is not editable and not deletable twice. Deployment and Gateway
// Key updates always answered 404 here; Route, Project and Provider did not,
// so a deleted object stayed writable with its post-delete ETag and the audit
// trail recorded successful mutations against a removed record.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTombstonedRouteRefusesUpdateAndSecondDelete(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	ctx := context.Background()

	project, err := runtime.store.GetProject(ctx, bootstrap.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	project.AllowedModels = nil
	if _, err := runtime.store.PutProject(ctx, project, project.Revision, nil); err != nil {
		t.Fatal(err)
	}
	route, err := runtime.store.GetRoute(ctx, bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	if recorder := deleteRouteRequest(t, runtime, session, route); recorder.Code != http.StatusNoContent {
		t.Fatalf("route delete: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	tombstone, err := runtime.store.GetRoute(ctx, bootstrap.RouteID)
	if err != nil || tombstone.DeletedAt == nil {
		t.Fatalf("route not tombstoned: err=%v", err)
	}

	update := adminMutationRequest(t, http.MethodPut, "/admin/api/v1/routes/"+tombstone.ID, session, map[string]any{
		"public_model": "renamed-after-death", "deployment_id": tombstone.DeploymentID,
		"priority": 5, "strategy": "ordered", "enabled": false,
	})
	update.Header.Set("If-Match", revisionETag(tombstone.Revision))
	recorder := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(recorder, update)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("update on tombstoned route: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := deleteRouteRequest(t, runtime, session, tombstone); recorder.Code != http.StatusNotFound {
		t.Fatalf("second delete on tombstoned route: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	stored, err := runtime.store.GetRoute(ctx, tombstone.ID)
	if err != nil || stored.Revision != tombstone.Revision || stored.PublicModel == "renamed-after-death" {
		t.Fatalf("tombstone was mutated: revision=%d public_model=%q err=%v", stored.Revision, stored.PublicModel, err)
	}
}

func TestTombstonedProjectRefusesUpdateAndSecondDelete(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	ctx := context.Background()

	project, err := runtime.store.GetProject(ctx, bootstrap.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	deleteReq := adminMutationRequest(t, http.MethodDelete, "/admin/api/v1/projects/"+project.ID, session, stepUp())
	deleteReq.Header.Set("If-Match", revisionETag(project.Revision))
	deleteRec := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("project delete: status=%d body=%s", deleteRec.Code, deleteRec.Body.String())
	}
	tombstone, err := runtime.store.GetProject(ctx, project.ID)
	if err != nil || tombstone.DeletedAt == nil {
		t.Fatalf("project not tombstoned: err=%v", err)
	}

	update := adminMutationRequest(t, http.MethodPut, "/admin/api/v1/projects/"+tombstone.ID, session, map[string]any{
		"name": "renamed after death", "enabled": false,
		"allowed_models": []string{}, "rpm": 1, "tpm": 1, "max_concurrency": 1,
		"daily_budget_micros_usd": 1, "max_input_tokens": 1, "max_output_tokens": 1,
		"max_request_bytes": 1, "max_stream_duration_seconds": 1, "allowed_cidrs": []string{},
	})
	update.Header.Set("If-Match", revisionETag(tombstone.Revision))
	recorder := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(recorder, update)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("update on tombstoned project: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	second := adminMutationRequest(t, http.MethodDelete, "/admin/api/v1/projects/"+tombstone.ID, session, stepUp())
	second.Header.Set("If-Match", revisionETag(tombstone.Revision))
	secondRec := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(secondRec, second)
	if secondRec.Code != http.StatusNotFound {
		t.Fatalf("second delete on tombstoned project: status=%d body=%s", secondRec.Code, secondRec.Body.String())
	}
}

func TestTombstonedProviderRefusesUpdateAndSecondDelete(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	ctx := context.Background()

	// Tear the chain down bottom-up so the provider becomes deletable.
	project, err := runtime.store.GetProject(ctx, bootstrap.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	project.AllowedModels = nil
	if _, err := runtime.store.PutProject(ctx, project, project.Revision, nil); err != nil {
		t.Fatal(err)
	}
	route, err := runtime.store.GetRoute(ctx, bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	if recorder := deleteRouteRequest(t, runtime, session, route); recorder.Code != http.StatusNoContent {
		t.Fatalf("route delete: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	deployment, err := runtime.store.GetDeployment(ctx, bootstrap.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	deploymentDelete := adminMutationRequest(t, http.MethodDelete, "/admin/api/v1/deployments/"+deployment.ID, session, stepUp())
	deploymentDelete.Header.Set("If-Match", revisionETag(deployment.Revision))
	deploymentRec := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(deploymentRec, deploymentDelete)
	if deploymentRec.Code != http.StatusNoContent {
		t.Fatalf("deployment delete: status=%d body=%s", deploymentRec.Code, deploymentRec.Body.String())
	}
	instance, err := runtime.store.GetProvider(ctx, bootstrap.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	providerDelete := adminMutationRequest(t, http.MethodDelete, "/admin/api/v1/providers/"+instance.ID, session, stepUp())
	providerDelete.Header.Set("If-Match", revisionETag(instance.Revision))
	providerRec := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(providerRec, providerDelete)
	if providerRec.Code != http.StatusNoContent {
		t.Fatalf("provider delete: status=%d body=%s", providerRec.Code, providerRec.Body.String())
	}
	tombstone, err := runtime.store.GetProvider(ctx, instance.ID)
	if err != nil || tombstone.DeletedAt == nil {
		t.Fatalf("provider not tombstoned: err=%v", err)
	}

	update := adminMutationRequest(t, http.MethodPut, "/admin/api/v1/providers/"+tombstone.ID, session, map[string]any{
		"name": "renamed after death", "type": string(tombstone.Type),
		"base_url": tombstone.BaseURL, "credential_id": tombstone.CredentialID,
		"enabled": false, "max_concurrency": 1,
		"capabilities": tombstone.Capabilities,
	})
	update.Header.Set("If-Match", revisionETag(tombstone.Revision))
	recorder := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(recorder, update)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("update on tombstoned provider: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	second := adminMutationRequest(t, http.MethodDelete, "/admin/api/v1/providers/"+tombstone.ID, session, stepUp())
	second.Header.Set("If-Match", revisionETag(tombstone.Revision))
	secondRec := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(secondRec, second)
	if secondRec.Code != http.StatusNotFound {
		t.Fatalf("second delete on tombstoned provider: status=%d body=%s", secondRec.Code, secondRec.Body.String())
	}
}
