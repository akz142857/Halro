package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/alert"
	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/safetransport"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/tokenguard"
	"github.com/akz142857/Halro/internal/vault"
)

func TestRuntimeLoadsAudienceBoundEncryptedWebhook(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	store, secretVault := openMetadataVault(t, cfg)
	policy := safetransport.Policy{RequireHTTPS: true, AllowedHosts: []string{"hooks.example"}}
	audience, err := safetransport.AudienceWithPolicy(
		"https://hooks.example/alerts", "webhook:Authorization", policy,
	)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := secretVault.EncryptCredential(
		"cred_webhook", webhookCredentialType, audience, []byte("Bearer webhook-secret"),
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	credential := domain.Credential{
		ID: "cred_webhook", Name: "Webhook", Type: domain.ProviderType(webhookCredentialType),
		Audience: audience, Ciphertext: ciphertext, KeyVersion: 1, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := store.PutCredential(context.Background(), credential, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutAlertWebhook(context.Background(), domain.AlertWebhook{
		ID: "webhook_1", Name: "Operations", URL: "https://hooks.example/alerts",
		AllowedHosts: []string{"hooks.example"}, HeaderName: "Authorization",
		CredentialID: credential.ID, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}, 0); err != nil {
		t.Fatal(err)
	}
	secretVault.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
}

func TestRuntimeRejectsPrivateWebhookEndpointByDefault(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	store, secretVault := openMetadataVault(t, cfg)
	now := time.Now().UTC()
	if _, err := store.PutAlertWebhook(context.Background(), domain.AlertWebhook{
		ID: "webhook_1", Name: "Private", URL: "https://127.0.0.1/alerts",
		AllowedHosts: []string{"127.0.0.1"}, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}, 0); err != nil {
		t.Fatal(err)
	}
	secretVault.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("private webhook endpoint was accepted")
	}
}

func TestAlertAuditStoresOnlyStableMetadata(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	canary := "sk-alert-payload-canary-0123456789"
	runtime.auditAlertSubmission(alert.Event{
		ID: "alert_1", Type: canary, ProjectID: "project_1",
		Summary: canary, Details: map[string]any{"secret": canary},
	}, alert.SubmissionResult{Accepted: false, Status: alert.SubmissionDropped})
	runtime.auditAlertDelivery(alert.DeliveryResult{
		EventID: "alert_1", EventType: canary, ProjectID: "project_1",
		EndpointID: "webhook_1", Outcome: "failure", Reason: "retry_exhausted",
		Attempts: 3, OccurredAt: time.Now().UTC(),
	})
	var generated, submitted, delivered bool
	if _, err := runtime.audit.Replay(func(record audit.Record) error {
		switch record.Event.Action {
		case "alert.generated":
			generated = record.Event.ReasonCode == "other"
		case "alert.submission":
			submitted = record.Event.Outcome == "failure" &&
				record.Event.ReasonCode == string(alert.SubmissionDropped)
		case "alert.delivery":
			delivered = record.Event.ActorID == "alert_1" &&
				record.Event.TargetID == "webhook_1" &&
				record.Event.ReasonCode == "retry_exhausted"
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !generated || !submitted || !delivered {
		t.Fatalf("missing alert audit events generated=%v submitted=%v delivered=%v",
			generated, submitted, delivered)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(cfg.AuditPath())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte(canary)) {
		t.Fatal("alert payload leaked into audit log")
	}
	if _, err := VerifyAudit(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
}

func TestTokenGuardAlertPipelineWritesGeneratedAndSubmissionAudit(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.PutTokenGuardPolicy(context.Background(), domain.TokenGuardPolicy{
		ID: "guard_1", Name: "Guard", Enabled: true, Action: "alert",
		RequestTokens: 1, CreatedAt: now, UpdatedAt: now,
	}, 0); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	decision := runtime.tokenGuard.Admit(tokenguard.Input{
		PolicyID: "guard_1", ProjectID: "project_1", KeyID: "key_1",
		EstimatedTokens: 2, Now: now,
	})
	if !decision.Allowed || decision.Reason != "request_tokens" {
		t.Fatalf("unexpected token guard decision: %#v", decision)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var generated, submitted bool
		if _, err := runtime.audit.Replay(func(record audit.Record) error {
			generated = generated || record.Event.Action == "alert.generated"
			submitted = submitted || record.Event.Action == "alert.submission"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if generated && submitted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("token guard alert audit pipeline timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func openMetadataVault(t *testing.T, cfg config.Config) (*boltstore.Store, *vault.Vault) {
	t.Helper()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	masterKey, err := vault.LoadMasterKey(cfg.Storage.MasterKey.File)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	secretVault, err := vault.New(masterKey)
	clear(masterKey)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	return store, secretVault
}
