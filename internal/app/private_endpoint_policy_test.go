package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

// allow_private_provider_endpoints could not take effect. Every provider-side
// path validated the URL with the configured policy and then derived the
// audience with safetransport.Audience, which substituted an empty policy and
// refused private addresses regardless — so the opt-in was cancelled at each of
// its own call sites, and a self-hosted model server could not be reached at all.
func TestPrivateProviderEndpointIsUsableWhenConfigurationAllowsIt(t *testing.T) {
	cfg := testConfig(t)
	cfg.Security.AllowPrivateProviderEndpoints = true
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)

	credResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
			"name": "Local", "type": "openai", "base_url": "https://10.1.2.3", "secret": "sk-local",
		})
	if credResponse.Code != http.StatusCreated {
		t.Fatalf("credential on a private endpoint: %d %s", credResponse.Code, credResponse.Body.String())
	}
	var credential domain.Credential
	if err := json.Unmarshal(credResponse.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}
	providerResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": "Local", "type": "openai", "base_url": "https://10.1.2.3",
			"credential_id": credential.ID, "enabled": true,
		})
	if providerResponse.Code != http.StatusCreated {
		t.Fatalf("provider on a private endpoint: %d %s", providerResponse.Code, providerResponse.Body.String())
	}
}

// The switch still has to mean something: with it off, the same request is
// refused. Otherwise the fix would have widened the SSRF surface unconditionally.
func TestPrivateProviderEndpointStaysRefusedByDefault(t *testing.T) {
	cfg := testConfig(t)
	cfg.Security.AllowPrivateProviderEndpoints = false
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)

	response := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
			"name": "Local", "type": "openai", "base_url": "https://10.1.2.3", "secret": "sk-local",
		})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("a private endpoint was accepted with the switch off: %d %s", response.Code, response.Body.String())
	}
}

// The other half of the same change. Now that a private-endpoint provider can
// exist, tightening the switch makes its stored endpoint illegal — and that must
// exclude the provider, not stop the process from starting. This is the scenario
// F-06 described; before F-07 it was unreachable because the provider could not
// be created.
func TestTighteningTheEndpointPolicyExcludesTheProviderRatherThanTheProcess(t *testing.T) {
	cfg := testConfig(t)
	cfg.Security.AllowPrivateProviderEndpoints = true
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)

	credResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
			"name": "Local", "type": "openai", "base_url": "https://10.1.2.3", "secret": "sk-local",
		})
	var credential domain.Credential
	if err := json.Unmarshal(credResponse.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}
	providerResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
			"name": "Local", "type": "openai", "base_url": "https://10.1.2.3",
			"credential_id": credential.ID, "enabled": true,
		})
	if providerResponse.Code != http.StatusCreated {
		t.Fatalf("provider setup: %d %s", providerResponse.Code, providerResponse.Body.String())
	}
	var instance domain.ProviderInstance
	if err := json.Unmarshal(providerResponse.Body.Bytes(), &instance); err != nil {
		t.Fatal(err)
	}
	runtime.Close()

	// The operator tightens the policy. Nothing stored changes.
	tightened := cfg
	tightened.Security.AllowPrivateProviderEndpoints = false

	reopened, err := Open(context.Background(), tightened, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("tightening a security setting stopped the process from starting: %v", err)
	}
	defer reopened.Close()

	// The unrelated public provider from bootstrap still routes.
	if targets := reopened.providers.ResolveCandidatesForEvidence("chat", provider.OperationChat, domain.EvidenceDeclared); len(targets) != 1 {
		t.Fatalf("an unrelated route was taken down with the excluded provider: %+v", targets)
	}
	// And the offending provider is reported, not silently dropped.
	_, report, err := loadProviderRegistry(context.Background(), tightened, reopened.store, reopened.vault)
	if err != nil {
		t.Fatalf("load after tightening: %v", err)
	}
	found := false
	for _, item := range report.Excluded {
		if item.ProviderID == instance.ID && item.Reason == excludedEndpointRejected {
			found = true
		}
	}
	if !found {
		t.Fatalf("the excluded provider was not reported: %+v", report.Excluded)
	}

	// Reported in the load report is not the same as visible to an operator.
	// The loader is deliberately tolerant here, so doctor is the one place the
	// exclusion can still be found without reading logs.
	reopened.Close()
	doctor, doctorErr := DoctorWithOptions(context.Background(), tightened, DoctorOptions{NoKMS: true})
	if doctorErr == nil {
		t.Fatal("doctor reported a healthy instance while a provider was excluded from routing")
	}
	named := false
	for _, check := range doctor.Checks {
		if check.Name == "topology" && check.Status == "fail" && strings.Contains(check.Detail, instance.ID) {
			named = true
		}
	}
	if !named {
		t.Fatalf("doctor did not name the excluded provider: %+v", doctor.Checks)
	}
}

func openRuntimeWithPolicyForTest(t *testing.T, cfg config.Config) (*Runtime, BootstrapResult) {
	t.Helper()
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte(stepUpTestPassword)); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test", PublicModel: "chat",
		ProjectName: "Policy", BillingMode: domain.BillingModeFree,
	}, []byte("provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	return runtime, bootstrap
}
