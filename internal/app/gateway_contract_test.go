package app

import (
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/akz142857/Halro/internal/compatibility"
)

// The gateway route table against the northbound profiles, in both directions.
//
// This is the first of the three steps docs/contracts/adding-a-northbound-endpoint.md
// names as having no mechanical guard, and it names it as the highest-value
// thing anyone adding an endpoint could leave behind. Nothing held the router to
// BuiltinNorthboundProfile's method lists, so two failures were possible and
// neither was visible:
//
//   - A route served with no profile and no manifest. It is then invisible to
//     every compatibility guard and absent from the published contract, while
//     answering real traffic. That is the same shape as the seventh registration
//     in adding-a-platform.md — a list that had to agree with another list, with
//     nothing holding them together — which was found by an operator hitting it.
//   - A method declared with no route. The manifest then promises an endpoint
//     that 404s, and docs/compatibility/endpoint-manifests.json is what an
//     integrator reads to decide whether their client will work.
//
// The admin router has had this guard since it was frozen (chi.Walk over
// adminRouter in admin_contract_test.go). The gateway router is the one that
// serves the contract, and did not.
func TestEveryGatewayRouteIsADeclaredNorthboundMethod(t *testing.T) {
	served, _ := walkGatewayRoutes(t)
	declared := declaredNorthboundMethods()

	for _, route := range served {
		if _, ok := declared[route]; !ok {
			t.Errorf("the gateway serves %q and no northbound profile declares it: add it to a profile's Methods in internal/compatibility/profile.go and give it an endpoint manifest, or it answers traffic no published contract describes", route)
		}
	}
	for route, profile := range declared {
		if !slices.Contains(served, route) {
			t.Errorf("northbound profile %s declares %q and the gateway router does not serve it: the manifest promises an endpoint that 404s", profile, route)
		}
	}
}

// Registered inside the guarded group, which is step 6's one warning and has no
// guard of its own either. The middlewares are what turn an unauthenticated
// caller away before its body is read; a route registered beside them instead of
// inside them parses first and authenticates second, so an anonymous caller sets
// the parsing cost and the per-project limiter that would bound it does not yet
// apply.
//
// Middleware values are functions and cannot be compared, so this counts them:
// the router itself carries two (panic recovery, the write deadline) and each
// guarded group adds three (the stale-snapshot refusal, the limiter, the guard).
// Counting is enough to catch the mistake the warning is about — registering a
// northbound route on the bare router — without pinning which three they are.
func TestEveryGatewayRouteSitsInsideItsGuardedGroup(t *testing.T) {
	served, middlewares := walkGatewayRoutes(t)
	declared := declaredNorthboundMethods()
	const baseMiddlewares = 2
	for _, route := range served {
		if _, ok := declared[route]; !ok {
			continue
		}
		if middlewares[route] <= baseMiddlewares {
			t.Errorf("%q is registered outside a guarded group (%d middlewares, the bare router has %d): an unauthenticated caller's body is read before it is turned away",
				route, middlewares[route], baseMiddlewares)
		}
	}
	// The assertion above is only worth something while the bare router really
	// does carry fewer. Health is deliberately outside the guarded groups — an
	// orchestrator probes it on a fixed interval and limiting by source would
	// eventually mark a healthy instance unready — so it is the witness.
	if count, ok := middlewares["GET /health/live"]; !ok || count != baseMiddlewares {
		t.Fatalf("GET /health/live carries %d middlewares, want the bare router's %d — the count above no longer separates guarded from unguarded", count, baseMiddlewares)
	}
}

// walkGatewayRoutes returns the routes the gateway serves, with chi's parameter
// names normalised to the profiles' own spelling, and how many middlewares each
// carries.
//
// Routes that are not an API face are excluded by name rather than by pattern,
// because "everything under /v1" would quietly re-admit the case this file
// exists to catch. Health and the root banner are the whole list, and adding to
// it is a deliberate act.
func walkGatewayRoutes(t *testing.T) ([]string, map[string]int) {
	t.Helper()
	runtime := &Runtime{}
	routes, ok := runtime.gatewayRouter().(chi.Routes)
	if !ok {
		t.Fatal("gateway router does not expose chi route metadata")
	}
	parameter := regexp.MustCompile(`\{[^}]+\}`)
	served := make([]string, 0)
	middlewares := make(map[string]int)
	if err := chi.Walk(routes, func(method, path string, _ http.Handler, chain ...func(http.Handler) http.Handler) error {
		route := method + " " + parameter.ReplaceAllString(path, "{id}")
		middlewares[route] = len(chain)
		switch route {
		case "GET /health/live", "GET /health/ready", "GET /":
			return nil
		}
		served = append(served, route)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(served)
	return served, middlewares
}

func declaredNorthboundMethods() map[string]compatibility.NorthboundProfileID {
	declared := make(map[string]compatibility.NorthboundProfileID)
	for _, profile := range compatibility.AllNorthboundProfiles() {
		for _, method := range profile.Methods {
			declared[strings.TrimSpace(method)] = profile.ID
		}
	}
	return declared
}
