package app

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
)

func TestCreateAndDisableProjectKeyOffline(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test",
		PublicModel: "chat", ProjectName: "Default",
	}, []byte("provider-key"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := CreateProjectKey(context.Background(), cfg, bootstrap.ProjectID, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.auth.Authenticate(created.GatewayKey, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err := DisableProjectKey(context.Background(), cfg, created.KeyID); err != nil {
		t.Fatal(err)
	}
	runtime, err = Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if _, err := runtime.auth.Authenticate(created.GatewayKey, time.Now()); err == nil {
		t.Fatal("disabled key remained valid after snapshot reload")
	}
}
