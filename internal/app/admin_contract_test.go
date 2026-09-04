package app

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestFrozenV1AdminRoutesAreRegistered(t *testing.T) {
	runtime := &Runtime{}
	routes, ok := runtime.adminRouter().(chi.Routes)
	if !ok {
		t.Fatal("admin router does not expose chi route metadata")
	}
	parameter := regexp.MustCompile(`\{[^}]+\}`)
	registered := make(map[string]struct{})
	if err := chi.Walk(routes, func(method, path string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		registered[method+" "+parameter.ReplaceAllString(path, "{}")] = struct{}{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, publicRouter := range []http.Handler{runtime.gatewayRouter(), runtime.metricsRouter()} {
		if err := chi.Walk(publicRouter.(chi.Routes), func(method, path string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			registered[method+" "+path] = struct{}{}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	expected := []string{
		"POST /v1/chat/completions", "POST /v1/embeddings",
		"GET /health/live", "GET /health/ready", "GET /metrics",
		"GET /admin/api/v1/setup/status", "POST /admin/api/v1/setup/admin",
		"POST /admin/api/v1/session/login", "POST /admin/api/v1/session/logout",
		"GET /admin/api/v1/session", "POST /admin/api/v1/session/password",
		"POST /admin/api/v1/session/mfa/totp", "POST /admin/api/v1/session/mfa/recovery-code",
		"DELETE /admin/api/v1/session/mfa/challenge",
		"GET /admin/api/v1/security/mfa", "POST /admin/api/v1/security/mfa/authenticators",
		"POST /admin/api/v1/security/mfa/authenticators/{}/confirm", "PATCH /admin/api/v1/security/mfa/authenticators/{}", "DELETE /admin/api/v1/security/mfa/authenticators/{}",
		"DELETE /admin/api/v1/security/mfa/authenticators/{}/pending",
		"POST /admin/api/v1/security/mfa/recovery-codes/regenerate", "DELETE /admin/api/v1/security/mfa",
		"GET /admin/api/v1/credentials", "POST /admin/api/v1/credentials",
		"GET /admin/api/v1/credentials/{}", "PUT /admin/api/v1/credentials/{}", "DELETE /admin/api/v1/credentials/{}",
		"GET /admin/api/v1/providers", "POST /admin/api/v1/providers",
		"GET /admin/api/v1/providers/{}", "PUT /admin/api/v1/providers/{}", "DELETE /admin/api/v1/providers/{}", "POST /admin/api/v1/providers/{}/test",
		"POST /admin/api/v1/providers/{}/model-capability-detections",
		"GET /admin/api/v1/model-capability-detections/{}", "DELETE /admin/api/v1/model-capability-detections/{}",
		"GET /admin/api/v1/deployments", "POST /admin/api/v1/deployments",
		"GET /admin/api/v1/deployments/{}", "PUT /admin/api/v1/deployments/{}", "DELETE /admin/api/v1/deployments/{}", "POST /admin/api/v1/deployments/{}/test",
		"POST /admin/api/v1/deployments/{}/capabilities/preflight",
		"GET /admin/api/v1/deployments/{}/prices", "POST /admin/api/v1/deployments/{}/prices", "POST /admin/api/v1/deployments/{}/prices/preview", "POST /admin/api/v1/deployments/{}/prices/restore-confirm", "POST /admin/api/v1/deployments/{}/prices/{}/cancel",
		"GET /admin/api/v1/deployments/{}/price-proposals", "POST /admin/api/v1/deployments/{}/price-proposals", "POST /admin/api/v1/deployments/{}/price-proposals/{}/adopt", "POST /admin/api/v1/deployments/{}/price-proposals/{}/reject",
		"GET /admin/api/v1/routes", "POST /admin/api/v1/routes",
		"GET /admin/api/v1/routes/{}", "PUT /admin/api/v1/routes/{}", "DELETE /admin/api/v1/routes/{}", "POST /admin/api/v1/routes/{}/test",
		"GET /admin/api/v1/projects", "POST /admin/api/v1/projects",
		"GET /admin/api/v1/projects/{}", "PUT /admin/api/v1/projects/{}", "DELETE /admin/api/v1/projects/{}", "POST /admin/api/v1/projects/{}/unblock",
		"GET /admin/api/v1/projects/{}/keys", "POST /admin/api/v1/projects/{}/keys", "DELETE /admin/api/v1/projects/{}/keys/{}",
		"GET /admin/api/v1/run-governance/work-units", "GET /admin/api/v1/run-governance/work-units/{}",
		"GET /admin/api/v1/run-governance/runs", "GET /admin/api/v1/run-governance/runs/{}",
		"GET /admin/api/v1/redaction-policies", "POST /admin/api/v1/redaction-policies",
		"GET /admin/api/v1/redaction-policies/{}", "PUT /admin/api/v1/redaction-policies/{}", "DELETE /admin/api/v1/redaction-policies/{}", "POST /admin/api/v1/redaction-policies/{}/test",
		"GET /admin/api/v1/token-guard-policies", "POST /admin/api/v1/token-guard-policies",
		"GET /admin/api/v1/token-guard-policies/{}", "PUT /admin/api/v1/token-guard-policies/{}", "DELETE /admin/api/v1/token-guard-policies/{}", "POST /admin/api/v1/token-guard-policies/{}/test",
		"GET /admin/api/v1/dashboard", "GET /admin/api/v1/onboarding/readiness", "GET /admin/api/v1/usage", "GET /admin/api/v1/usage/requests/{}",
		"GET /admin/api/v1/usage/summary", "GET /admin/api/v1/usage/failures", "GET /admin/api/v1/usage/failures/{}/payload",
		"GET /admin/api/v1/settings/usage", "PUT /admin/api/v1/settings/usage",
		"GET /admin/api/v1/alerts", "POST /admin/api/v1/alerts/test", "GET /admin/api/v1/audit", "GET /admin/api/v1/system/status",
		"GET /admin/api/v1/developer/config", "POST /admin/api/v1/developer/execute/{}",
		// Routes that were served without ever being frozen. The list was
		// one-way until this round, so each of these was added and nothing
		// noticed; the two-way check below is what found them.
		"GET /admin/api/v1/admin-users", "POST /admin/api/v1/admin-users", "DELETE /admin/api/v1/admin-users/{}",
		"POST /admin/api/v1/alerts", "DELETE /admin/api/v1/alerts/{}", "GET /admin/api/v1/alerts/{}", "PUT /admin/api/v1/alerts/{}", "POST /admin/api/v1/alerts/{}/test",
		"GET /admin/api/v1/master-key/custody", "GET /admin/api/v1/master-key/runbooks/lifecycle", "GET /admin/api/v1/master-key/runbooks/recovery",
		"GET /admin/api/v1/model-catalog", "POST /admin/api/v1/model-catalog/refresh",
		"GET /admin/api/v1/preferences", "PUT /admin/api/v1/preferences",
		"GET /admin/api/v1/projects/{}/keys/{}", "PUT /admin/api/v1/projects/{}/keys/{}",
		"GET /admin/api/v1/provider-profiles",
		"GET /admin/api/v1/providers/{}/invocation-targets", "POST /admin/api/v1/providers/{}/invocation-targets",
		"GET /admin/api/v1/runbooks/configuration-stale", "GET /admin/api/v1/runbooks/file-master-key-rotation", "GET /admin/api/v1/runbooks/gateway-key-compromise",
		"GET /admin/api/v1/settings", "PUT /admin/api/v1/settings", "GET /admin/api/v1/settings/accounting", "PUT /admin/api/v1/settings/accounting", "DELETE /admin/api/v1/settings/accounting/pending", "GET /admin/api/v1/settings/ui", "PUT /admin/api/v1/settings/ui",
		"GET /admin/api/v1/system/config",
		"GET /admin/api/v1/ui/bootstrap",
	}
	frozen := make(map[string]struct{}, len(expected))
	for _, route := range expected {
		frozen[route] = struct{}{}
		if _, exists := registered[route]; !exists {
			t.Errorf("frozen v1 route is not registered: %s", route)
		}
	}
	// The other direction, which this check was missing. A one-way list says
	// "everything frozen is still served" and is silent about a route that was
	// added and never frozen — so the list fell behind by one endpoint family in
	// v0.5.0 and by four more in v0.6.0, and nothing failed. The gateway router
	// grew a two-way contract test this round; this is its Admin counterpart.
	//
	// Adding a route to this list is the deliberate act: it is a v1 surface from
	// then on, and the compatibility promise attaches to it.
	// Scoped to the Admin API itself: the gateway's own routes have their own
	// two-way contract test, and the console's static routes are a wildcard
	// serving a bundle rather than an interface anyone integrates against.
	for route := range registered {
		method, path, found := strings.Cut(route, " ")
		if !found || !strings.HasPrefix(path, "/admin/api/v1/") || strings.Contains(path, "*") {
			continue
		}
		if _, isFrozen := frozen[route]; !isFrozen {
			t.Errorf("registered Admin API route is not in the frozen v1 list: %s %s", method, path)
		}
	}
}
