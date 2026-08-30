package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

func createRouteRequest(t *testing.T, runtime *Runtime, session loggedInAdmin, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	request := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/routes", session, body)
	recorder := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(recorder, request)
	return recorder
}

// Two enabled routes on one alias pointing at the same deployment look like a
// fallback pair and are not one: they share the credential, the endpoint and the
// upstream quota, and the circuit breaker keys on the route ID, so it does not
// merge them either. Nothing refused this, and the console counted it as two
// targets.
func TestASecondEnabledRouteToTheSameDeploymentIsRefused(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	route, err := runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}

	recorder := createRouteRequest(t, runtime, session, map[string]any{
		"public_model": route.PublicModel, "deployment_id": route.DeploymentID,
		"priority": route.Priority + 10, "strategy": route.Strategy, "enabled": true,
	})

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	routes, err := runtime.store.ListRoutes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("a refused create still wrote a route: %+v", routes)
	}
}

// The check has to be about the deployment, not about the alias: a second target
// on a different deployment is the configuration the whole feature exists for.
func TestASecondEnabledRouteToAnotherDeploymentOnTheSameAliasIsAllowed(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	route, err := runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	sibling := cloneDeployment(t, runtime, bootstrap.DeploymentID, "dep-sibling")

	recorder := createRouteRequest(t, runtime, session, map[string]any{
		"public_model": route.PublicModel, "deployment_id": sibling.ID,
		"priority": route.Priority + 10, "strategy": route.Strategy, "enabled": true,
	})

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// A disabled duplicate is a maintenance state, not a fake fallback pair, and the
// same shape the strategy check already has. Enabling it later is an update,
// which runs the same validator and is refused there.
func TestADisabledDuplicateRouteIsAllowedAndCannotBeEnabled(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	route, err := runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"public_model": route.PublicModel, "deployment_id": route.DeploymentID,
		"priority": route.Priority + 10, "strategy": route.Strategy, "enabled": false,
	}
	recorder := createRouteRequest(t, runtime, session, body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("creating the disabled duplicate: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var created domain.Route
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	body["enabled"] = true
	update := adminMutationRequest(t, http.MethodPut, "/admin/api/v1/routes/"+created.ID, session, body)
	update.Header.Set("If-Match", revisionETag(created.Revision))
	enabled := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(enabled, update)

	if enabled.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", enabled.Code, enabled.Body.String())
	}
}

func cloneDeployment(t *testing.T, runtime *Runtime, deploymentID, cloneID string) domain.Deployment {
	t.Helper()
	deployment, err := runtime.store.GetDeployment(context.Background(), deploymentID)
	if err != nil {
		t.Fatal(err)
	}
	clone := deployment
	clone.ID, clone.Name, clone.Revision = cloneID, deployment.Name+" (sibling)", 0
	stored, err := runtime.store.PutDeployment(context.Background(), clone, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

// The console read `enabled` from the store, which says whether the operator
// switched the route on — not whether it reached routing. A withheld route was
// shown as Enabled while it served nothing, and only the log said otherwise.
func TestAWithheldRouteIsReportedAsWithheldByTheRoutesList(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	switchOffDeploymentBinding(t, runtime, bootstrap.ProviderID, bootstrap.DeploymentID)
	if err := runtime.reloadProviderRegistry(context.Background()); err != nil {
		t.Fatalf("hot reload refused to complete: %v", err)
	}

	listed := listRoutes(t, runtime, session)
	if len(listed) != 1 {
		t.Fatalf("routes=%+v", listed)
	}
	if listed[0].Withheld == nil {
		t.Fatalf("the withheld route is still reported as plain Enabled: %+v", listed[0])
	}
	if listed[0].Kind() != routeWithheldReference || listed[0].Reason() != withheldBindingUnavailable {
		t.Fatalf("withheld=%+v", listed[0])
	}
	if !listed[0].Enabled {
		t.Fatalf("withholding must not rewrite the stored state: %+v", listed[0])
	}
}

// A route that routes says nothing, and a disabled route is switched off rather
// than withheld — the console already has a word for that one.
func TestARoutingRouteCarriesNoWithholding(t *testing.T) {
	runtime, _, session := activationTestRuntime(t)

	listed := listRoutes(t, runtime, session)
	if len(listed) != 1 {
		t.Fatalf("routes=%+v", listed)
	}
	if listed[0].Withheld != nil {
		t.Fatalf("a routing route was reported as withheld: %+v", listed[0])
	}
}

type listedRoute struct {
	ID       string `json:"id"`
	Enabled  bool   `json:"enabled"`
	Withheld *struct {
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
	} `json:"withheld"`
}

func (r listedRoute) Kind() string {
	if r.Withheld == nil {
		return ""
	}
	return r.Withheld.Kind
}

func (r listedRoute) Reason() string {
	if r.Withheld == nil {
		return ""
	}
	return r.Withheld.Reason
}

func listRoutes(t *testing.T, runtime *Runtime, session loggedInAdmin) []listedRoute {
	t.Helper()
	request := adminMutationRequest(t, http.MethodGet, "/admin/api/v1/routes", session, nil)
	recorder := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var decoded struct {
		Items []listedRoute `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded.Items
}
