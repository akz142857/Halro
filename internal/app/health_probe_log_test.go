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

// The periodic probe is the one that runs unattended, on every enabled
// deployment, for as long as the process lives. A manual connection test writes
// the upstream's sentence to the operator's screen and nowhere else; this loop
// used to write the classified error straight into the log, so the same
// sentence — including any credential the upstream echoed back inside it —
// landed on disk once per probe interval and stayed there.
//
// Both paths now log the same field set, and this test exists because they are
// reached through different code and a green suite for one says nothing about
// the other.
func TestPeriodicDeploymentProbeLogsNoUpstreamBody(t *testing.T) {
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
		ID       string `json:"id"`
		Bindings []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		} `json:"bindings"`
	}
	if providerResponse.Code != http.StatusCreated || json.Unmarshal(providerResponse.Body.Bytes(), &instance) != nil {
		t.Fatalf("provider status=%d body=%s", providerResponse.Code, providerResponse.Body.String())
	}
	deploymentResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/deployments", "",
		map[string]any{
			"name": "gpt-test", "provider_id": instance.ID, "provider_model": "gpt-test",
			"target_kind": "model_id", "mode": "operator_declared",
			"capabilities": map[string]any{"chat": true}, "enabled": false,
		},
	)
	var deployment struct {
		ID string `json:"id"`
	}
	if deploymentResponse.Code != http.StatusCreated || json.Unmarshal(deploymentResponse.Body.Bytes(), &deployment) != nil {
		t.Fatalf("deployment status=%d body=%s", deploymentResponse.Code, deploymentResponse.Body.String())
	}
	// Only enabled deployments are probed, and enabling reloads the registry —
	// so the adapter that answers the probe is registered after, not before.
	enableStoredDeploymentForTest(t, runtime, deployment.ID)

	adapter := &failingProbeAdapter{err: &provider.Error{
		Class:             provider.ErrorAuthentication,
		StatusCode:        http.StatusForbidden,
		ProviderCode:      "AccessDeniedException",
		ProviderRequestID: "req-42",
		Message:           "provider error (403): not authorized to call this project with " + bedrockKeyCanary,
	}}
	registry := provider.NewRegistry()
	for _, binding := range instance.Bindings {
		if !binding.Enabled {
			continue
		}
		if err := registry.RegisterBindingAdapter(instance.ID, binding.ID, adapter); err != nil {
			t.Fatal(err)
		}
	}
	runtime.providers.Replace(registry)

	logs.Reset()
	runtime.probeDeployments(context.Background())

	logged := logs.String()
	// The probe has to still be reported, and reported with enough to act on —
	// a test that only asserts the absence of the sentence is satisfied by a
	// loop that logs nothing at all.
	if !strings.Contains(logged, "active deployment probe failed") ||
		!strings.Contains(logged, `"error_class":"authentication"`) ||
		!strings.Contains(logged, `"provider_status":403`) ||
		!strings.Contains(logged, "AccessDeniedException") ||
		!strings.Contains(logged, "req-42") {
		t.Fatalf("periodic probe failure was not logged with its classification: %s", logged)
	}
	if strings.Contains(logged, "not authorized to call this project") {
		t.Fatalf("an upstream response body was written to the log: %s", logged)
	}
	if strings.Contains(logged, bedrockKeyCanary) {
		t.Fatalf("periodic probe log leaked a credential: %s", logged)
	}

	// The counterpart the manual path also draws: nothing reached an upstream,
	// so the sentence is one Halro wrote, and dropping it would leave the line
	// unable to say which address was refused.
	logs.Reset()
	adapter.err = &provider.Error{Class: provider.ErrorConnect, Message: "provider probe failed", Cause: fmt.Errorf(
		"dial: %w: reserved address 198.18.4.6 is not allowed", safetransport.ErrRefusedBeforeSend)}
	runtime.probeDeployments(context.Background())

	if refused := logs.String(); !strings.Contains(refused, `"reason"`) || !strings.Contains(refused, "198.18.4.6") {
		t.Fatalf("a refusal Halro produced itself was not logged with its cause: %s", refused)
	}
}
