package app

import (
	"context"
	"io"
	"log/slog"
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
	audience, err := safetransport.Audience("https://api.openai.com", string(domain.ProviderOpenAI))
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
	if _, err := store.PutCredential(context.Background(), credential, 0); err != nil {
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
	if _, err := store.PutProvider(context.Background(), instance, 0); err != nil {
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
		Weight: 1, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := store.PutDeployment(context.Background(), deployment, 0); err != nil {
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
	if _, err := store.PutRoute(context.Background(), route, 0); err != nil {
		t.Fatal(err)
	}
}
