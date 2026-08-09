package app

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/domain"
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
		BillingMode: domain.BillingModeFree,
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

	// The two ways this break-glass path differs from the Admin API's delete.
	// docs/runbooks/gateway-key-compromise.md §2.5 tells an operator both, so
	// they are asserted rather than left to the reader of keys.go.
	//
	// It leaves no tombstone, so a directory revoked this way still looks
	// untouched by the §1 "zero records means rebuilt" diagnostic...
	disabled, err := runtime.store.GetGatewayKey(context.Background(), created.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Enabled {
		t.Fatal("the CLI left the key enabled")
	}
	if disabled.DeletedAt != nil {
		t.Fatal("the CLI now stamps DeletedAt; §2.5 says it does not, and the runbook needs updating")
	}

	// ...and it audits under a different action, so an operator verifying
	// revocation by the API's name would find nothing and redo the work.
	var actions []string
	if _, err := runtime.audit.Replay(func(record audit.Record) error {
		actions = append(actions, record.Event.Action)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(actions, "gateway_key.disable") {
		t.Fatalf("no gateway_key.disable in the audit trail; got %v", actions)
	}
}
