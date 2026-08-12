package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.etcd.io/bbolt"

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

	// Declared capabilities, so the accept cases are answered by the region rule
	// alone. Asserting only "not refused for that reason" would pass while every
	// create failed for some other one, which is what it used to do.
	deployment := func(name, region string) *httptest.ResponseRecorder {
		input := map[string]any{
			"name": name, "provider_id": instance.ID,
			"provider_model": "anthropic.claude-test", "target_kind": domain.TargetModelID,
			"mode":         "operator_declared",
			"capabilities": map[string]any{"chat": true, "streaming": true, "stream_usage": true},
			"enabled":      false,
		}
		if region != "" {
			input["region"] = region
		}
		return performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/deployments", "", input)
	}

	const rejection = "region must match the Bedrock Mantle endpoint region"
	if body := deployment("foreign region", "eu-west-1").Body.String(); !strings.Contains(body, rejection) {
		t.Fatalf("a deployment in a region the endpoint cannot reach was not refused for that reason: %s", body)
	}
	// The matching region and the omitted one are created, not merely refused for
	// some other reason.
	if response := deployment("declared region", "us-east-1"); response.Code != http.StatusCreated {
		t.Fatalf("a deployment declaring its own endpoint region was refused: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := deployment("derived region", ""); response.Code != http.StatusCreated {
		t.Fatalf("a deployment deriving its region from the endpoint was refused: status=%d body=%s", response.Code, response.Body.String())
	}

	// Capability detection accepts the same field, keys the same catalog and the
	// stored evidence on it, and pays for billable probes to produce it. A
	// detection stamped with a region no deployment can adopt is evidence that
	// can only ever be discarded.
	detection := func(region string) *httptest.ResponseRecorder {
		input := map[string]any{
			"provider_model": "anthropic.claude-test", "target_kind": domain.TargetModelID,
			"risk_tier": "safe_automatic",
			// Detection spends the Provider credential, so it carries step-up.
			"current_password": stepUpTestPassword,
		}
		if region != "" {
			input["region"] = region
		}
		return performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
			"/admin/api/v1/providers/"+instance.ID+"/model-capability-detections", "", input)
	}
	foreign := detection("eu-west-1")
	if foreign.Code != http.StatusBadRequest || !strings.Contains(foreign.Body.String(), rejection) {
		t.Fatalf("a detection in a region the endpoint cannot reach was not refused for that reason: status=%d body=%s",
			foreign.Code, foreign.Body.String())
	}
	if body := detection("us-east-1").Body.String(); strings.Contains(body, rejection) {
		t.Fatalf("a detection declaring its own endpoint region was refused: %s", body)
	}
	if body := detection("").Body.String(); strings.Contains(body, rejection) {
		t.Fatalf("a detection deriving its region from the endpoint was refused: %s", body)
	}
}

// The Admin API refuses a mismatched region, but a restore replaces the whole
// data directory without replaying that path, and a provider's endpoint can be
// moved under a deployment that already exists. The loader answers that state
// the same way it answers an over-ceiling binding: withhold the one route and
// keep loading everything else.
func TestStoredMantleDeploymentWithAForeignRegionIsWithheldNotFatal(t *testing.T) {
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
	var instance struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(providerResponse.Body.Bytes(), &instance); err != nil {
		t.Fatal(err)
	}
	deploymentResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/deployments", "", map[string]any{
		"name": "mantle deployment", "provider_id": instance.ID, "provider_model": "anthropic.claude-test",
		"mode":         "operator_declared",
		"capabilities": map[string]any{"chat": true, "streaming": true, "stream_usage": true},
		"enabled":      false,
	})
	if deploymentResponse.Code != http.StatusCreated {
		t.Fatalf("deployment setup: status=%d body=%s", deploymentResponse.Code, deploymentResponse.Body.String())
	}
	var deployment struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(deploymentResponse.Body.Bytes(), &deployment); err != nil {
		t.Fatal(err)
	}
	enableStoredDeploymentForTest(t, runtime, deployment.ID)
	routeResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/routes", "", map[string]any{
		"public_model": "mantle-chat", "deployment_id": deployment.ID,
		"priority": 10, "strategy": "ordered", "enabled": true,
	})
	if routeResponse.Code != http.StatusCreated {
		t.Fatalf("route setup: status=%d body=%s", routeResponse.Code, routeResponse.Body.String())
	}
	runtime.Close()

	// Only the deployment's region is rewritten: everything else stays exactly
	// as the Admin API wrote it, so nothing but the rule under test can be the
	// reason the route is withheld.
	rewriteStoredDeploymentRegion(t, cfg.MetadataPath(), deployment.ID, "eu-west-1")

	reopened, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("a mismatched Mantle region stopped the process from starting: %v", err)
	}
	defer reopened.Close()

	_, report, err := loadProviderRegistry(context.Background(), cfg, reopened.store, reopened.vault)
	if err != nil {
		t.Fatalf("load with a mismatched region: %v", err)
	}
	withheld := false
	for _, item := range report.Dangling {
		if item.DeploymentID == deployment.ID && item.Reason == withheldRegionMismatched {
			withheld = true
		}
	}
	if !withheld {
		t.Fatalf("the mismatched deployment was not withheld: %+v", report.Dangling)
	}
}

func rewriteStoredDeploymentRegion(t *testing.T, path, deploymentID, region string) {
	t.Helper()
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rewrote := false
	if err := db.Update(func(tx *bbolt.Tx) error {
		deployments := tx.Bucket([]byte("deployments"))
		raw := deployments.Get([]byte(deploymentID))
		if raw == nil {
			return nil
		}
		var deployment domain.Deployment
		if err := json.Unmarshal(raw, &deployment); err != nil {
			return err
		}
		deployment.Region = region
		encoded, err := json.Marshal(deployment)
		if err != nil {
			return err
		}
		rewrote = true
		return deployments.Put([]byte(deploymentID), encoded)
	}); err != nil {
		t.Fatal(err)
	}
	if !rewrote {
		t.Fatal("the stored deployment was not found, so its region was never changed")
	}
}
