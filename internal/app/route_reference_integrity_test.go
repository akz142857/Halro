package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

func deleteRouteRequest(t *testing.T, runtime *Runtime, session loggedInAdmin, route domain.Route) *httptest.ResponseRecorder {
	t.Helper()
	request := adminMutationRequest(t, http.MethodDelete, "/admin/api/v1/routes/"+route.ID, session, stepUp())
	request.Header.Set("If-Match", revisionETag(route.Revision))
	recorder := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(recorder, request)
	return recorder
}

// Project writes reject an unknown alias, because an alias with no route fails
// only at request time and does so silently. The reverse direction had no such
// check, so deleting the last route for an alias left the Project authorizing
// something that could only answer 404 — and, worse, made the Project unsaveable,
// since its own validator then rejects the alias it already holds.
func TestDeletingTheLastRouteForAReferencedAliasIsRefused(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	route, err := runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}

	recorder := deleteRouteRequest(t, runtime, session, route)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var decoded map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["code"] != "route_referenced_by_project" {
		t.Fatalf("code=%q error=%q", decoded["code"], decoded["error"])
	}

	// A refusal that already tombstoned the route is not a refusal.
	stored, err := runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DeletedAt != nil || !stored.Enabled {
		t.Fatalf("route was mutated by a refused delete: deleted=%v enabled=%v", stored.DeletedAt, stored.Enabled)
	}
	// And the project it protects is still editable, which is the symptom that
	// made this more than a cosmetic error code.
	project, err := runtime.store.GetProject(context.Background(), bootstrap.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"name": project.Name, "allowed_routes": project.AllowedRoutes, "enabled": true,
		"daily_budget_micros_usd": project.DailyBudgetMicrosUSD,
		"rpm":                     project.RPM, "tpm": project.TPM,
		"max_concurrency": project.MaxConcurrency,
	}
	update := adminMutationRequest(t, http.MethodPut, "/admin/api/v1/projects/"+project.ID, session, body)
	update.Header.Set("If-Match", revisionETag(project.Revision))
	updated := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(updated, update)
	if updated.Code != http.StatusOK {
		t.Fatalf("the project could not be re-saved: status=%d body=%s", updated.Code, updated.Body.String())
	}
}

// Removing one of several routes on an alias is a capacity change, not an
// outage, and must stay allowed or the guard would make fallback routes
// undeletable.
func TestDeletingOneOfSeveralRoutesForAnAliasIsAllowed(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	first, err := runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	sibling := first
	sibling.ID, sibling.Priority, sibling.Revision = "rte-sibling", first.Priority+10, 0
	if _, err := runtime.store.PutRoute(context.Background(), sibling, 0, nil); err != nil {
		t.Fatal(err)
	}

	if recorder := deleteRouteRequest(t, runtime, session, first); recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// Nothing references the alias, so there is no integrity to protect.
func TestDeletingTheLastRouteForAnUnreferencedAliasIsAllowed(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	project, err := runtime.store.GetProject(context.Background(), bootstrap.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	project.AllowedRoutes = nil
	if _, err := runtime.store.PutProject(context.Background(), project, project.Revision, nil); err != nil {
		t.Fatal(err)
	}
	route, err := runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}

	if recorder := deleteRouteRequest(t, runtime, session, route); recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// Renaming the only route for an alias removes that alias just as deleting the
// route would. Guarding delete alone would leave the same dangling reference
// reachable through the edit form.
func TestRenamingTheLastRouteForAReferencedAliasIsRefused(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	route, err := runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"public_model": "chat-renamed", "deployment_id": route.DeploymentID,
		"priority": route.Priority, "strategy": route.Strategy, "enabled": true,
	}
	request := adminMutationRequest(t, http.MethodPut, "/admin/api/v1/routes/"+route.ID, session, body)
	request.Header.Set("If-Match", revisionETag(route.Revision))
	recorder := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	stored, err := runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PublicModel != route.PublicModel {
		t.Fatalf("alias was renamed by a refused update: %q", stored.PublicModel)
	}
}

// Disabling a route is deliberately still allowed: a disabled route stays
// bindable and the console shows it as unavailable, which is the maintenance
// state the review's Q-01 kept open on purpose.
func TestDisablingTheLastRouteForAReferencedAliasIsAllowed(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	route, err := runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"public_model": route.PublicModel, "deployment_id": route.DeploymentID,
		"priority": route.Priority, "strategy": route.Strategy, "enabled": false,
	}
	request := adminMutationRequest(t, http.MethodPut, "/admin/api/v1/routes/"+route.ID, session, body)
	request.Header.Set("If-Match", revisionETag(route.Revision))
	recorder := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
