package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
)

func TestBootstrapCreatesUsableConfigurationWithoutPersistingPlaintextSecrets(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	providerSecret := []byte("provider-secret-never-persist")
	result, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName:           "OpenAI",
		ProviderType:           domain.ProviderOpenAI,
		ProviderBaseURL:        "https://api.openai.com",
		ProviderModel:          "gpt-test",
		PublicModel:            "chat",
		ProjectName:            "Default",
		DailyBudgetMicrosUSD:   1_000_000,
		InputMicrosPerMillion:  1_000,
		OutputMicrosPerMillion: 2_000,
	}, providerSecret)
	if err != nil {
		t.Fatal(err)
	}
	if result.GatewayKey == "" {
		t.Fatal("gateway key was not returned")
	}
	rawMetadata, err := os.ReadFile(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range [][]byte{providerSecret, []byte(result.GatewayKey)} {
		if bytes.Contains(rawMetadata, secret) {
			t.Fatal("plaintext secret was persisted in metadata")
		}
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	principal, err := runtime.auth.Authenticate(result.GatewayKey, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if principal.Project.ID != result.ProjectID {
		t.Fatalf("unexpected project: %#v", principal.Project)
	}
	if _, ok := runtime.providers.Resolve("chat"); !ok {
		t.Fatal("bootstrap route was not loaded")
	}
}
