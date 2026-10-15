package app

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/safetransport"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

func TestRuntimeLoadsEncryptedProviderAndRouteThroughSafePolicy(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	seedProvider(t, cfg, false)
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	target, ok := runtime.providers.Resolve("chat")
	if !ok || target.ProviderModel != "gpt-test" || target.Adapter.Type() != "openai" {
		t.Fatalf("unexpected target: %#v ok=%v", target, ok)
	}
}

func TestRuntimeRejectsCredentialAudienceMismatch(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	seedProvider(t, cfg, true)
	if _, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("expected audience mismatch to fail startup")
	}
}

func seedProvider(t *testing.T, cfg config.Config, mismatch bool) {
	t.Helper()
	masterKey, err := vault.LoadMasterKey(cfg.Storage.MasterKey.File)
	if err != nil {
		t.Fatal(err)
	}
	secretVault, err := vault.New(masterKey)
	clear(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer secretVault.Close()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	audience, err := safetransport.AudienceWithPolicy("https://api.openai.com", string(domain.ProviderOpenAI), safetransport.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	encryptionAudience := audience
	if mismatch {
		audience = "https://other.example:443:openai"
	}
	ciphertext, err := secretVault.EncryptCredential(
		"cred_1",
		string(domain.ProviderOpenAI),
		encryptionAudience,
		[]byte("provider-key"),
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	profile, _ := domain.DefaultProviderProfile(domain.ProviderOpenAI)
	capabilities := domain.DefaultProviderCapabilities(domain.ProviderOpenAI)
	credential := domain.Credential{
		ID:         "cred_1",
		Name:       "OpenAI",
		Type:       domain.ProviderOpenAI,
		Audience:   audience,
		Ciphertext: ciphertext,
		KeyVersion: 1,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if _, err := store.PutCredential(context.Background(), credential, 0, nil); err != nil {
		t.Fatal(err)
	}
	instance := domain.ProviderInstance{
		ID:            "provider_1",
		Name:          "OpenAI",
		Type:          domain.ProviderOpenAI,
		BaseURL:       "https://api.openai.com",
		CredentialID:  credential.ID,
		AllowedHosts:  []string{"api.openai.com"},
		Enabled:       true,
		CreatedAt:     now,
		UpdatedAt:     now,
		AccessSurface: profile.AccessSurface, ProfileID: profile.ProfileID,
		CredentialScheme: profile.CredentialScheme, Capabilities: capabilities,
		CapabilityEvidence: domain.EvidenceForCapabilities(capabilities, domain.EvidenceDeclared),
	}
	if _, err := store.PutProvider(context.Background(), instance, 0, nil); err != nil {
		t.Fatal(err)
	}
	// A route names a deployment; provider, model, price, capabilities and
	// concurrency all come from it.
	deployment := domain.Deployment{
		ID: "deployment_1", Name: "OpenAI / gpt-test", ProviderID: instance.ID,
		ProviderModel: "gpt-test", AccessSurface: instance.AccessSurface,
		ProfileID: instance.ProfileID, Capabilities: capabilities,
		CapabilityEvidence: domain.EvidenceForCapabilities(capabilities, domain.EvidenceDeclared),
		ModelCapabilitySnapshot: domain.DeclaredCapabilitySnapshot(
			"gpt-test", "sha256:test", capabilities, now),
		Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := store.PutDeployment(context.Background(), deployment, 0, nil); err != nil {
		t.Fatal(err)
	}
	route := domain.Route{
		ID:           "route_1",
		PublicModel:  "chat",
		DeploymentID: deployment.ID,
		Enabled:      true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if _, err := store.PutRoute(context.Background(), route, 0, nil); err != nil {
		t.Fatal(err)
	}
}

// Model discovery reads a host the provider record does not name. The signer is
// already pinned to it, but the transport policy was built from the record
// alone, so the request was signed correctly and then refused by Halro's own
// dialer — the model list came back empty with nothing in it to explain why.
//
// This asserts the client that is actually built, not a policy value on its way
// there: the allowlist a binding runs under is exactly what nothing could read
// back before, and it is where the defect lived.
func TestBedrockRuntimeClientMayDialTheDerivedControlPlaneOnly(t *testing.T) {
	cfg := testConfig(t)
	runtimeEndpoint, _ := url.Parse("https://bedrock-runtime.us-east-1.amazonaws.com")
	runtimeBinding := domain.ProviderProfileBinding{AccessSurface: domain.SurfaceBedrockRuntime}
	base := safetransport.Policy{RequireHTTPS: true, AllowedHosts: []string{"bedrock-runtime.us-east-1.amazonaws.com"}}

	client, err := newBindingClient(cfg, runtimeBinding, runtimeEndpoint, base)
	if err != nil {
		t.Fatal(err)
	}
	policy, ok := safetransport.PolicyOf(client)
	if !ok {
		t.Fatal("a client built by SafeTransport did not report its policy")
	}
	if !slices.Contains(policy.AllowedHosts, "bedrock.us-east-1.amazonaws.com") {
		t.Fatalf("the control plane host is not dialable: %v", policy.AllowedHosts)
	}
	if len(policy.AllowedHosts) != 2 {
		t.Fatalf("the policy was widened by more than the derived host: %v", policy.AllowedHosts)
	}
	if len(base.AllowedHosts) != 1 {
		t.Fatalf("the caller's policy was mutated: %v", base.AllowedHosts)
	}

	// Nothing else derives one. An agent-runtime host is a different service, and
	// a private endpoint has no public control plane to reach.
	for _, host := range []string{
		"https://bedrock-agent-runtime.us-east-1.amazonaws.com",
		"https://vpce-0123-abcd.bedrock-runtime.us-east-1.vpce.amazonaws.com",
	} {
		other, _ := url.Parse(host)
		narrow, err := newBindingClient(cfg, runtimeBinding, other, safetransport.Policy{RequireHTTPS: true, AllowedHosts: []string{other.Hostname()}})
		if err != nil {
			t.Fatal(err)
		}
		if policy, _ := safetransport.PolicyOf(narrow); len(policy.AllowedHosts) != 1 {
			t.Fatalf("%s widened the policy: %v", host, policy.AllowedHosts)
		}
	}

	// A binding on another access surface is left alone even when its host would
	// derive one: the control plane belongs to the runtime surface.
	mantle := domain.ProviderProfileBinding{AccessSurface: domain.SurfaceBedrockMantle}
	other, err := newBindingClient(cfg, mantle, runtimeEndpoint, base)
	if err != nil {
		t.Fatal(err)
	}
	if policy, _ := safetransport.PolicyOf(other); len(policy.AllowedHosts) != 1 {
		t.Fatalf("a non-runtime surface widened the policy: %v", policy.AllowedHosts)
	}
}
