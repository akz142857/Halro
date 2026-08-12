package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

// A Bedrock Project is a region-scoped resource — its ARN carries the region —
// and model availability, quota and capability evidence are region-scoped with
// it. deploymentRegion lets an explicitly declared region win over the one the
// provider endpoint points at, which on Mantle would key the catalog and the
// stored evidence on a region no request can reach.
func TestMantleDeploymentRegionMustMatchTheEndpointRegion(t *testing.T) {
	cfg := testConfig(t)
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)
	endpoint := "https://bedrock-mantle.us-east-1.api.aws"

	credentialResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "Mantle API key", "type": "bedrock", "base_url": endpoint, "secret": "bedrock-api-key",
		"access_surface": domain.SurfaceBedrockMantle, "scheme": domain.CredentialBedrockAPIKey,
	})
	var credential struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(credentialResponse.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}
	providerResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
		"name": "mantle", "type": "bedrock", "base_url": endpoint,
		"credential_id": credential.ID, "enabled": true,
		"access_surface": domain.SurfaceBedrockMantle, "profile_id": domain.ProfileBedrockMantleAnthropicMessages,
		"credential_scheme": domain.CredentialBedrockAPIKey,
	})
	if providerResponse.Code != http.StatusCreated {
		t.Fatalf("provider setup: status=%d body=%s", providerResponse.Code, providerResponse.Body.String())
	}
	var instance domain.ProviderInstance
	if err := json.Unmarshal(providerResponse.Body.Bytes(), &instance); err != nil {
		t.Fatal(err)
	}

	deployment := func(region string) string {
		input := map[string]any{
			"name": "mantle deployment", "provider_id": instance.ID,
			"provider_model": "anthropic.claude-test", "target_kind": domain.TargetModelID,
			"enabled": false,
		}
		if region != "" {
			input["region"] = region
		}
		return performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/deployments", "", input).Body.String()
	}

	const rejection = "region must match the Bedrock Mantle endpoint region"
	if body := deployment("eu-west-1"); !strings.Contains(body, rejection) {
		t.Fatalf("a deployment in a region the endpoint cannot reach was not refused for that reason: %s", body)
	}
	// The matching region and the omitted one are answered by the rest of the
	// create path, whatever it decides — the region rule must not be what stops
	// them, or it would have made the surface unusable rather than consistent.
	if body := deployment("us-east-1"); strings.Contains(body, rejection) {
		t.Fatalf("a deployment declaring its own endpoint region was refused: %s", body)
	}
	if body := deployment(""); strings.Contains(body, rejection) {
		t.Fatalf("a deployment deriving its region from the endpoint was refused: %s", body)
	}
}
