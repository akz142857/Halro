package app

import (
	"net/http"
	"regexp"
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
		"POST /admin/api/v1/session/login", "POST /admin/api/v1/session/logout",
		"GET /admin/api/v1/session", "POST /admin/api/v1/session/password",
		"GET /admin/api/v1/credentials", "POST /admin/api/v1/credentials",
		"GET /admin/api/v1/credentials/{}", "PUT /admin/api/v1/credentials/{}", "DELETE /admin/api/v1/credentials/{}",
		"GET /admin/api/v1/providers", "POST /admin/api/v1/providers",
		"GET /admin/api/v1/providers/{}", "PUT /admin/api/v1/providers/{}", "DELETE /admin/api/v1/providers/{}", "POST /admin/api/v1/providers/{}/test",
		"GET /admin/api/v1/deployments", "POST /admin/api/v1/deployments",
		"GET /admin/api/v1/deployments/{}", "PUT /admin/api/v1/deployments/{}", "DELETE /admin/api/v1/deployments/{}", "POST /admin/api/v1/deployments/{}/test",
		"GET /admin/api/v1/routes", "POST /admin/api/v1/routes",
		"GET /admin/api/v1/routes/{}", "PUT /admin/api/v1/routes/{}", "DELETE /admin/api/v1/routes/{}", "POST /admin/api/v1/routes/{}/test",
		"GET /admin/api/v1/projects", "POST /admin/api/v1/projects",
		"GET /admin/api/v1/projects/{}", "PUT /admin/api/v1/projects/{}", "DELETE /admin/api/v1/projects/{}", "POST /admin/api/v1/projects/{}/unblock",
		"GET /admin/api/v1/projects/{}/keys", "POST /admin/api/v1/projects/{}/keys", "DELETE /admin/api/v1/projects/{}/keys/{}",
		"GET /admin/api/v1/redaction-policies", "POST /admin/api/v1/redaction-policies",
		"GET /admin/api/v1/redaction-policies/{}", "PUT /admin/api/v1/redaction-policies/{}", "DELETE /admin/api/v1/redaction-policies/{}", "POST /admin/api/v1/redaction-policies/{}/test",
		"GET /admin/api/v1/token-guard-policies", "POST /admin/api/v1/token-guard-policies",
		"GET /admin/api/v1/token-guard-policies/{}", "PUT /admin/api/v1/token-guard-policies/{}", "DELETE /admin/api/v1/token-guard-policies/{}", "POST /admin/api/v1/token-guard-policies/{}/test",
		"GET /admin/api/v1/dashboard", "GET /admin/api/v1/usage", "GET /admin/api/v1/usage/requests/{}",
		"GET /admin/api/v1/alerts", "POST /admin/api/v1/alerts/test", "GET /admin/api/v1/audit", "GET /admin/api/v1/system/status",
	}
	for _, route := range expected {
		if _, exists := registered[route]; !exists {
			t.Errorf("frozen v1 route is not registered: %s", route)
		}
	}
}
