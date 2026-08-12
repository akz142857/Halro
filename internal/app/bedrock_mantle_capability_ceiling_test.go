package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"go.etcd.io/bbolt"

	"github.com/akz142857/Halro/internal/domain"
)

// The Bedrock Mantle profiles are Beta and their capability set is fixed by the
// build. The Admin API held the only list of immutable profiles and the Mantle
// ones were absent from it, so this request used to succeed: the provider was
// stored advertising embeddings and reasoning the profile cannot serve, and
// everything downstream — capability detection plans, the data-plane preflight
// — read the stored claim and believed it.
func TestAdminRefusesMantleCapabilitiesAboveTheProfileCeiling(t *testing.T) {
	cfg := testConfig(t)
	runtime, _ := openRuntimeWithPolicyForTest(t, cfg)
	cookie, csrf := loginAdminForTest(t, runtime)
	endpoint := "https://bedrock-mantle.us-east-1.api.aws"

	credentialResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "Mantle API key", "type": "bedrock", "base_url": endpoint, "secret": "bedrock-api-key",
		"access_surface": domain.SurfaceBedrockMantle, "scheme": domain.CredentialBedrockAPIKey,
	})
	if credentialResponse.Code != http.StatusCreated {
		t.Fatalf("credential create status=%d body=%s", credentialResponse.Code, credentialResponse.Body.String())
	}
	var credential struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(credentialResponse.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		profile domain.ProviderProfileID
		widen   func(*domain.ProviderCapabilities)
	}{
		{"chat claims embeddings", domain.ProfileBedrockMantleOpenAIChat, func(c *domain.ProviderCapabilities) { c.Embeddings = true }},
		{"responses claims reasoning", domain.ProfileBedrockMantleOpenAIResponses, func(c *domain.ProviderCapabilities) { c.Reasoning = true }},
		{"messages claims json mode", domain.ProfileBedrockMantleAnthropicMessages, func(c *domain.ProviderCapabilities) { c.JSONMode = true }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			capabilities := domain.DefaultProviderCapabilitiesForProfile(domain.ProviderBedrock, test.profile)
			test.widen(&capabilities)
			response := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
				"name": "widened " + string(test.profile), "type": "bedrock", "base_url": endpoint,
				"credential_id": credential.ID, "enabled": true,
				"access_surface": domain.SurfaceBedrockMantle, "profile_id": test.profile,
				"credential_scheme": domain.CredentialBedrockAPIKey, "capabilities": capabilities,
			})
			if response.Code != http.StatusBadRequest {
				t.Fatalf("widened %s provider was accepted: status=%d body=%s", test.profile, response.Code, response.Body.String())
			}
		})
	}

	// The ceiling itself still saves, or the check would have made the profile
	// unusable rather than bounded.
	atCeiling := domain.DefaultProviderCapabilitiesForProfile(domain.ProviderBedrock, domain.ProfileBedrockMantleOpenAIChat)
	accepted := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
		"name": "at ceiling", "type": "bedrock", "base_url": endpoint,
		"credential_id": credential.ID, "enabled": true,
		"access_surface": domain.SurfaceBedrockMantle, "profile_id": domain.ProfileBedrockMantleOpenAIChat,
		"credential_scheme": domain.CredentialBedrockAPIKey, "capabilities": atCeiling,
	})
	if accepted.Code != http.StatusCreated {
		t.Fatalf("a Mantle provider at its own ceiling was refused: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
}

// A record written before the ceiling check existed cannot be rejected by the
// write path, because it is already on disk. The loader has to answer it, and
// the answer is to withhold that one binding — not to refuse the load. Refusing
// would turn one stale Mantle record into a process that cannot open a data
// directory it has already written, taking every unrelated provider with it.
func TestStoredMantleBindingAboveTheCeilingIsWithheldNotFatal(t *testing.T) {
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
	runtime.Close()

	// Widen the stored record behind the store's back. This is the only way to
	// produce the state a pre-check data directory can hold, and it is exactly
	// the state the loader must survive.
	widenStoredProviderCapabilities(t, cfg.MetadataPath(), instance.ID, func(stored *domain.ProviderInstance) {
		stored.Capabilities.Embeddings = true
		for index := range stored.Bindings {
			stored.Bindings[index].Capabilities.Embeddings = true
		}
	})

	reopened, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("a stale over-ceiling Mantle record stopped the process from starting: %v", err)
	}
	defer reopened.Close()

	_, report, err := loadProviderRegistry(context.Background(), cfg, reopened.store, reopened.vault)
	if err != nil {
		t.Fatalf("load with a stale over-ceiling record: %v", err)
	}
	withheld := false
	for _, item := range report.Excluded {
		if item.ProviderID == instance.ID && item.Reason == excludedCapabilityCeilingExceeded {
			withheld = true
		}
	}
	if !withheld {
		t.Fatalf("the over-ceiling binding was not withheld: %+v", report.Excluded)
	}
}

func widenStoredProviderCapabilities(t *testing.T, path, providerID string, widen func(*domain.ProviderInstance)) {
	t.Helper()
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	changed := false
	if err := db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte("providers"))
		if bucket == nil {
			return nil
		}
		raw := bucket.Get([]byte(providerID))
		if raw == nil {
			return nil
		}
		var stored domain.ProviderInstance
		if err := json.Unmarshal(raw, &stored); err != nil {
			return err
		}
		widen(&stored)
		encoded, err := json.Marshal(stored)
		if err != nil {
			return err
		}
		changed = true
		return bucket.Put([]byte(providerID), encoded)
	}); err != nil {
		t.Fatal(err)
	}
	// Without this the test would "pass" against a record it never modified.
	if !changed {
		t.Fatal("the stored provider record was not found, so nothing was widened")
	}
}
