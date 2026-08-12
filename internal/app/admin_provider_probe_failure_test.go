package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/safelog"
	"github.com/akz142857/Halro/internal/safetransport"
)

// failingProbeAdapter answers a connection test the way an upstream refusal
// reaches the handler: a classified provider error carrying the status, the
// provider's own code and its sentence.
type failingProbeAdapter struct {
	canaryAdapter
	err error
}

func (a *failingProbeAdapter) Probe(context.Context, string) error { return a.err }

// A failed connection test used to leave the operator with a red "failed" in
// the console and nothing in the process log: the class was stored, the
// upstream's own answer was dropped on the floor, and the audit trail recorded
// only that a test had failed. Both ends now carry the reason.
func TestAdminProviderTestReportsAndLogsTheUpstreamReason(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(
		context.Background(), cfg, "admin", []byte("correct horse battery staple"),
	); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	// The real binary logs through safelog; a probe reason is upstream text, so
	// the test asserts against the same redacting handler production uses.
	runtime, err := Open(context.Background(), cfg, safelog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, csrf := loginAdminForTest(t, runtime)

	credentialResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/credentials", "",
		map[string]any{
			"name": "OpenAI production", "type": "openai",
			"base_url": "https://api.openai.com", "secret": "sk-provider-secret-canary",
		},
	)
	var credential credentialView
	if credentialResponse.Code != http.StatusCreated || json.Unmarshal(credentialResponse.Body.Bytes(), &credential) != nil {
		t.Fatalf("credential status=%d body=%s", credentialResponse.Code, credentialResponse.Body.String())
	}
	providerResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "",
		map[string]any{
			"name": "OpenAI", "type": "openai", "base_url": "https://api.openai.com",
			"credential_id": credential.ID, "enabled": true,
			"capabilities": map[string]any{"chat": true},
		},
	)
	var instance struct {
		ID       string `json:"id"`
		Bindings []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		} `json:"bindings"`
	}
	if providerResponse.Code != http.StatusCreated || json.Unmarshal(providerResponse.Body.Bytes(), &instance) != nil {
		t.Fatalf("provider status=%d body=%s", providerResponse.Code, providerResponse.Body.String())
	}

	registry := provider.NewRegistry()
	adapter := &failingProbeAdapter{err: &provider.Error{
		Class:             provider.ErrorAuthentication,
		StatusCode:        http.StatusForbidden,
		ProviderCode:      "AccessDeniedException",
		ProviderRequestID: "req-42",
		// The upstream sentence a real refusal carries, with the key it just
		// refused echoed back inside it. The canary is a Bedrock API key shape,
		// which safelog's pattern list does not know — the point being that the
		// log must not carry upstream body text at all rather than rely on
		// recognising every credential format the gateway can present.
		Message: "provider error (403): not authorized to call this project with " + bedrockKeyCanary,
	}}
	for _, binding := range instance.Bindings {
		if !binding.Enabled {
			continue
		}
		if err := registry.RegisterBindingAdapter(instance.ID, binding.ID, adapter); err != nil {
			t.Fatal(err)
		}
	}
	runtime.providers.Replace(registry)

	testResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers/"+instance.ID+"/test", "", nil,
	)
	if testResponse.Code != http.StatusBadGateway {
		t.Fatalf("provider test status=%d body=%s", testResponse.Code, testResponse.Body.String())
	}
	var result struct {
		Status         string `json:"status"`
		ErrorClass     string `json:"error_class"`
		ProviderStatus int    `json:"provider_status"`
		ProviderCode   string `json:"provider_code"`
		RequestID      string `json:"provider_request_id"`
		ErrorDetail    string `json:"error_detail"`
	}
	if err := json.Unmarshal(testResponse.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "unhealthy" || result.ErrorClass != string(provider.ErrorAuthentication) ||
		result.ProviderStatus != http.StatusForbidden || result.ProviderCode != "AccessDeniedException" ||
		result.RequestID != "req-42" || !strings.Contains(result.ErrorDetail, "not authorized to call this project") {
		t.Fatalf("test response did not explain the failure: %#v", result)
	}

	logged := logs.String()
	if !strings.Contains(logged, "provider connection test failed") ||
		!strings.Contains(logged, `"error_class":"authentication"`) ||
		!strings.Contains(logged, `"provider_status":403`) ||
		!strings.Contains(logged, "AccessDeniedException") {
		t.Fatalf("probe failure was not logged: %s", logged)
	}
	// The upstream's own sentence is a provider response body. It reaches the
	// operator who ran the test, in the reply, and it is not written to disk —
	// the one thing an upstream is most likely to quote is the credential it
	// just rejected, and a pattern denylist only knows the formats it was told
	// about.
	if strings.Contains(logged, "not authorized to call this project") {
		t.Fatalf("an upstream response body was written to the log: %s", logged)
	}
	if strings.Contains(logged, bedrockKeyCanary) {
		t.Fatalf("probe failure log leaked a credential: %s", logged)
	}

	// A transport refusal is the case the sentence was added for, and the one
	// case whose text Halro wrote itself: provider.Error.Error stops at its own
	// headline, so the address the dial was refused for never reached the
	// operator. It carries no provider body, so it is logged.
	logs.Reset()
	adapter.err = &provider.Error{Class: provider.ErrorConnect, Message: "provider probe failed", Cause: fmt.Errorf(
		"dial: %w: reserved address 198.18.4.6 is not allowed", safetransport.ErrRefusedBeforeSend)}
	refusalResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers/"+instance.ID+"/test", "", nil,
	)
	if refusalResponse.Code != http.StatusBadGateway ||
		!strings.Contains(refusalResponse.Body.String(), "198.18.4.6") ||
		!strings.Contains(refusalResponse.Body.String(), "provider probe failed") {
		t.Fatalf("transport refusal reason lost its cause: status=%d body=%s",
			refusalResponse.Code, refusalResponse.Body.String())
	}
	if refused := logs.String(); !strings.Contains(refused, `"reason"`) || !strings.Contains(refused, "198.18.4.6") {
		t.Fatalf("a refusal Halro produced itself was not logged with its cause: %s", refused)
	}
}

// A Bedrock API key shape, deliberately outside safelog's pattern list.
const bedrockKeyCanary = "ABSKQmVkcm9ja0FQSUtleUNhbmFyeQ=="

// The two identifiers come off an upstream body and an upstream header, both
// read under a megabyte limit, and both land in a log attribute and a console
// cell. A hostile or misconfigured host must not be able to write its own
// kilobytes there, and must not be able to smuggle a sentence through a field
// the console renders as an identifier.
func TestProbeIdentifiersAreBoundedAndNarrow(t *testing.T) {
	for _, test := range []struct {
		name     string
		code     string
		expected string
	}{
		{"a real provider code", "AccessDeniedException", "AccessDeniedException"},
		{"a request id", "req-42:0000-abc.1", "req-42:0000-abc.1"},
		{"surrounding whitespace", "  ThrottlingException\n", "ThrottlingException"},
		{"an oversized value", strings.Repeat("a", maxProbeIdentifierLength+1), ""},
		{"prose smuggled through an identifier", "not authorized with " + bedrockKeyCanary, ""},
		{"an empty value", "   ", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := describeProbeFailure(&provider.Error{
				Class: provider.ErrorAuthentication, StatusCode: http.StatusForbidden,
				ProviderCode: test.code, ProviderRequestID: test.code,
			})
			if failure.Code != test.expected || failure.RequestID != test.expected {
				t.Fatalf("code=%q request_id=%q want %q", failure.Code, failure.RequestID, test.expected)
			}
		})
	}
}

// The bound exists because the reason still reaches a console cell. A cut that
// lands inside a multibyte rune must not leave a replacement character behind.
func TestProbeReasonIsBoundedWithoutBreakingARune(t *testing.T) {
	for _, test := range []struct {
		name     string
		reason   string
		expected string
	}{
		{"under the bound", strings.Repeat("a", maxProbeReasonLength-1), strings.Repeat("a", maxProbeReasonLength-1)},
		{"at the bound", strings.Repeat("a", maxProbeReasonLength), strings.Repeat("a", maxProbeReasonLength)},
		{"over the bound", strings.Repeat("a", maxProbeReasonLength+1), strings.Repeat("a", maxProbeReasonLength) + "…"},
		{
			"a multibyte rune straddling the cut",
			strings.Repeat("a", maxProbeReasonLength-1) + "世界",
			strings.Repeat("a", maxProbeReasonLength-1) + "…",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := truncateProbeReason(test.reason); got != test.expected {
				t.Fatalf("truncateProbeReason(%d bytes) = %q want %q", len(test.reason), got, test.expected)
			}
		})
	}
}

// codedErrorBody's two reserved keys are what the console dispatches on: a
// field that overwrote `code` would degrade a localised refusal to the generic
// message, and one that overwrote `error` would replace the sentence under it.
func TestCodedErrorBodyKeepsItsReservedKeys(t *testing.T) {
	body := codedErrorBody("credential_base_url_mismatch", "the real sentence", map[string]string{
		"code": "overwritten", "error": "overwritten", "credential_base_url": "https://api.openai.com:443",
	})
	if body["code"] != "credential_base_url_mismatch" || body["error"] != "the real sentence" ||
		body["credential_base_url"] != "https://api.openai.com:443" {
		t.Fatalf("reserved keys were not preserved: %#v", body)
	}
}

// The deployment and route test endpoints carry the same reporting, and each
// has its own handler: the provider test passing says nothing about either.
// Removing the failure reporting from one of them used to leave the suite
// green.
func TestDeploymentAndRouteTestsReportTheSameFailure(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(
		context.Background(), cfg, "admin", []byte("correct horse battery staple"),
	); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	runtime, err := Open(context.Background(), cfg, safelog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, csrf := loginAdminForTest(t, runtime)

	credentialResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/credentials", "",
		map[string]any{
			"name": "OpenAI production", "type": "openai",
			"base_url": "https://api.openai.com", "secret": "sk-provider-secret-canary",
		},
	)
	var credential credentialView
	if credentialResponse.Code != http.StatusCreated || json.Unmarshal(credentialResponse.Body.Bytes(), &credential) != nil {
		t.Fatalf("credential status=%d body=%s", credentialResponse.Code, credentialResponse.Body.String())
	}
	providerResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "",
		map[string]any{
			"name": "OpenAI", "type": "openai", "base_url": "https://api.openai.com",
			"credential_id": credential.ID, "enabled": true,
			"capabilities": map[string]any{"chat": true},
		},
	)
	var instance struct {
		ID string `json:"id"`
	}
	if providerResponse.Code != http.StatusCreated || json.Unmarshal(providerResponse.Body.Bytes(), &instance) != nil {
		t.Fatalf("provider status=%d body=%s", providerResponse.Code, providerResponse.Body.String())
	}
	deploymentResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/deployments", "",
		map[string]any{
			"name": "gpt-test", "provider_id": instance.ID, "provider_model": "gpt-test",
			"target_kind": "model_id", "mode": "operator_declared",
			// New deployments are saved disabled and validated before enable; the
			// test endpoint is exactly what validates them, and it is what this
			// test is calling.
			"capabilities": map[string]any{"chat": true}, "enabled": false,
		},
	)
	var deployment struct {
		ID string `json:"id"`
	}
	if deploymentResponse.Code != http.StatusCreated || json.Unmarshal(deploymentResponse.Body.Bytes(), &deployment) != nil {
		t.Fatalf("deployment status=%d body=%s", deploymentResponse.Code, deploymentResponse.Body.String())
	}
	// A route only tests what it can serve, so the deployment behind it is priced
	// and enabled the way the store would hold it after a passing validation.
	enableStoredDeploymentForTest(t, runtime, deployment.ID)
	routeResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/routes", "",
		map[string]any{"public_model": "chat", "deployment_id": deployment.ID, "priority": 1, "enabled": true},
	)
	var route struct {
		ID string `json:"id"`
	}
	if routeResponse.Code != http.StatusCreated || json.Unmarshal(routeResponse.Body.Bytes(), &route) != nil {
		t.Fatalf("route status=%d body=%s", routeResponse.Code, routeResponse.Body.String())
	}

	failing := &failingProbeAdapter{err: &provider.Error{
		Class:             provider.ErrorAuthentication,
		StatusCode:        http.StatusForbidden,
		ProviderCode:      "AccessDeniedException",
		ProviderRequestID: "req-77",
		Message:           "provider error (403): not authorized to call this project with " + bedrockKeyCanary,
	}}
	for _, test := range []struct{ kind, path string }{
		{"deployment", "/admin/api/v1/deployments/" + deployment.ID + "/test"},
		{"route", "/admin/api/v1/routes/" + route.ID + "/test"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			registry := provider.NewRegistry()
			if err := registry.RegisterAdapter(instance.ID, failing); err != nil {
				t.Fatal(err)
			}
			runtime.providers.Replace(registry)
			logs.Reset()

			response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, test.path, "", nil)
			if response.Code != http.StatusBadGateway {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var result struct {
				ErrorClass     string `json:"error_class"`
				ProviderStatus int    `json:"provider_status"`
				ProviderCode   string `json:"provider_code"`
				RequestID      string `json:"provider_request_id"`
				ErrorDetail    string `json:"error_detail"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.ErrorClass != string(provider.ErrorAuthentication) || result.ProviderStatus != http.StatusForbidden ||
				result.ProviderCode != "AccessDeniedException" || result.RequestID != "req-77" ||
				!strings.Contains(result.ErrorDetail, "not authorized to call this project") {
				t.Fatalf("%s test response did not explain the failure: %#v", test.kind, result)
			}
			logged := logs.String()
			if !strings.Contains(logged, test.kind+" connection test failed") ||
				!strings.Contains(logged, `"provider_status":403`) {
				t.Fatalf("%s failure was not logged: %s", test.kind, logged)
			}
			if strings.Contains(logged, "not authorized to call this project") || strings.Contains(logged, bedrockKeyCanary) {
				t.Fatalf("%s log carried an upstream response body: %s", test.kind, logged)
			}
		})
	}
}
