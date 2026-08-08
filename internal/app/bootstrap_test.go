package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
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
	deployment, err := runtime.store.GetDeployment(context.Background(), result.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if deployment.InputMicrosPerMillion != 0 || deployment.OutputMicrosPerMillion != 0 {
		t.Fatalf("bootstrap persisted legacy deployment price fields: %#v", deployment)
	}
	price, err := runtime.store.SelectDeploymentPriceVersion(context.Background(), result.DeploymentID, time.Now().UTC())
	if err != nil || price.Version != 1 || price.BillingMode != domain.BillingModeMetered ||
		price.InputMicrosPerMillion != 1_000 || price.OutputMicrosPerMillion != 2_000 {
		t.Fatalf("bootstrap versioned price=%#v err=%v", price, err)
	}
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

func TestBootstrapRequiresExplicitFreeBillingModeForZeroPrices(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	_, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test",
		PublicModel: "chat", ProjectName: "Default",
	}, []byte("provider-secret"))
	if err == nil {
		t.Fatal("bootstrap inferred free billing from zero prices")
	}
}
