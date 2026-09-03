package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/safelog"
)

// countingProbeAdapter answers every probe successfully and says how often it
// was asked. A route the upstream refuses has to be caught before it is asked.
type countingProbeAdapter struct {
	canaryAdapter
	calls atomic.Int64
}

func (a *countingProbeAdapter) Probe(context.Context, string) error {
	a.calls.Add(1)
	return nil
}

// The unattended probe is the one an operator reads as "this deployment works".
// It dials the provider's model list, and on Bedrock Mantle that list
// enumerates the account rather than the route — so for a deployment naming an
// /openai/v1 model on the /v1 profile it answers, the probe reports healthy,
// and every request the same deployment serves comes back 400. The manual
// connection test and this loop are different code, so a green suite for one
// says nothing about the other.
func TestActiveProbeRefusesAModelTheRouteDoesNotServe(t *testing.T) {
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
			"name": "mantle", "type": "bedrock",
			"base_url":       "https://bedrock-mantle.us-east-2.api.aws",
			"access_surface": domain.SurfaceBedrockMantle, "scheme": domain.CredentialBedrockAPIKey,
			"secret": "test-secret",
		},
	)
	var credential credentialView
	if credentialResponse.Code != http.StatusCreated || json.Unmarshal(credentialResponse.Body.Bytes(), &credential) != nil {
		t.Fatalf("credential status=%d body=%s", credentialResponse.Code, credentialResponse.Body.String())
	}
	providerResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "",
		map[string]any{
			"name": "Bedrock Mantle", "type": "bedrock",
			"base_url":       "https://bedrock-mantle.us-east-2.api.aws",
			"access_surface": domain.SurfaceBedrockMantle, "profile_id": domain.ProfileBedrockMantleChat,
			"credential_id": credential.ID, "enabled": true,
			"capabilities": map[string]any{"chat": true},
		},
	)
	var instance struct {
		ID       string `json:"id"`
		Bindings []struct {
			ID        string                   `json:"id"`
			ProfileID domain.ProviderProfileID `json:"profile_id"`
			Enabled   bool                     `json:"enabled"`
		} `json:"bindings"`
	}
	if providerResponse.Code != http.StatusCreated || json.Unmarshal(providerResponse.Body.Bytes(), &instance) != nil {
		t.Fatalf("provider status=%d body=%s", providerResponse.Code, providerResponse.Body.String())
	}

	// Created disabled, which is the one way this deployment can exist at all:
	// enabling it through the API is refused now. That is the state an install
	// made before the refusal existed is in, and the state this test is about.
	deploymentResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/deployments", "",
		map[string]any{
			"name": "wrong-route", "provider_id": instance.ID,
			"provider_model": mantleOpenAIRouteModel, "target_kind": "model_id",
			"profile_id": domain.ProfileBedrockMantleChat, "region": "us-east-2",
			"mode": "operator_declared", "capabilities": map[string]any{"chat": true},
			"enabled": false,
		},
	)
	var deployment struct {
		ID string `json:"id"`
	}
	if deploymentResponse.Code != http.StatusCreated || json.Unmarshal(deploymentResponse.Body.Bytes(), &deployment) != nil {
		t.Fatalf("deployment status=%d body=%s", deploymentResponse.Code, deploymentResponse.Body.String())
	}
	enableStoredDeploymentForTest(t, runtime, deployment.ID)

	adapter := &countingProbeAdapter{}
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

	if calls := adapter.calls.Load(); calls != 0 {
		t.Fatalf("the probe dialled a route that refuses this model %d times", calls)
	}
	logged := logs.String()
	if !strings.Contains(logged, "active deployment probe failed") ||
		!strings.Contains(logged, string(domain.ProfileBedrockMantleOpenAIChat)) {
		t.Fatalf("the refusal was not logged with the profile that serves the model: %s", logged)
	}

	// And the same loop still dials where the catalogue has no objection: this
	// check must not turn every Mantle probe into a local refusal.
	stored, err := runtime.store.GetDeployment(context.Background(), deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.ProviderModel = "deepseek.v3.1"
	stored.ModelCapabilitySnapshot.ProviderModel = stored.ProviderModel
	if _, err := runtime.store.PutDeployment(context.Background(), stored, stored.Revision, nil); err != nil {
		t.Fatal(err)
	}
	runtime.probeDeployments(context.Background())
	if calls := adapter.calls.Load(); calls != 1 {
		t.Fatalf("a model this route serves was probed %d times, want 1", calls)
	}
}
