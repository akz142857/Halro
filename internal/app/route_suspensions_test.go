package app

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/routegate"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

// The whole path, offline and against a real data directory: a runtime learns a
// refusal, the row outlives it, the command shows the row, and the clear
// removes it and lands in the trusted audit chain.
//
// It is one test rather than four because the value is in the seam between the
// running gate and the offline tool — each half passes on its own with the
// other broken.
func TestOfflineRouteSuspensionListAndClear(t *testing.T) {
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

	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// The credential the bootstrap sealed, read back from the store rather than
	// invented: the suspension is keyed by it, and a made-up id would make the
	// restart assertion pass against a scope no target belongs to.
	credentials, err := runtime.store.ListCredentials(context.Background())
	if err != nil || len(credentials) != 1 {
		t.Fatalf("credentials=%+v err=%v", credentials, err)
	}
	credentialID := credentials[0].ID
	runtime.routes.Observe(provider.Target{
		ID: "route_1", DeploymentID: bootstrap.DeploymentID, ProviderID: bootstrap.ProviderID,
		ProviderModel: "gpt-test", CredentialID: credentialID, CredentialRevision: 1,
	}, routegate.Observation{
		Reason: provider.FailureReasonInvalidCredential,
		Class:  provider.ErrorAuthentication, Status: 401,
	}, time.Now().UTC())
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	items, err := ListStoredRouteSuspensions(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("stored suspensions = %+v, want the credential refusal", items)
	}
	item := items[0]
	if item.ScopeKind != "credential" || item.ScopeKey != credentialID || !item.Indefinite {
		t.Fatalf("stored %+v, want an indefinite credential suspension", item)
	}
	if strings.ContainsRune(item.ScopeKey, 0) {
		t.Fatalf("a NUL reached the command's output: %q", item.ScopeKey)
	}

	// A restart believes it: the gate is rebuilt from the store, not from
	// traffic it has not seen yet.
	restarted, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if live := restarted.routes.Snapshot(time.Now().UTC()); len(live) != 1 || !live[0].Indefinite {
		t.Fatalf("the restarted gate holds %+v, want the stored suspension", live)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}

	if err := ClearStoredRouteSuspension(context.Background(), cfg, item.ScopeID); err != nil {
		t.Fatal(err)
	}
	if items, err := ListStoredRouteSuspensions(context.Background(), cfg); err != nil || len(items) != 0 {
		t.Fatalf("items after the clear = %+v err=%v", items, err)
	}
	// Clearing is an administrative action, and the record has to be in the
	// same chain the Admin API writes to.
	auditKey := offlineAuditKey(t, cfg)
	log, err := audit.Open(cfg.AuditPath(), auditKey)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cleared := false
	if _, err := log.Replay(func(record audit.Record) error {
		if record.Event.Action == "route_suspension.clear" && record.Event.TargetID == item.ScopeID {
			cleared = true
			if record.Event.ActorType != "local_cli" {
				t.Errorf("actor type = %q, want local_cli", record.Event.ActorType)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !cleared {
		t.Fatal("the offline clear left no audit record")
	}

	// And the gate no longer believes it after the next start.
	reopened, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if live := reopened.routes.Snapshot(time.Now().UTC()); len(live) != 0 {
		t.Fatalf("a cleared suspension came back on restart: %+v", live)
	}
}

// A handle that is not one from the listing is refused before the data lock is
// taken, so a typo does not look like an empty result.
func TestClearingRejectsAHandleItDidNotIssue(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := ClearStoredRouteSuspension(context.Background(), cfg, "not-a-handle!!"); err == nil {
		t.Fatal("an unparseable scope handle was accepted")
	}
}

// offlineAuditKey opens the audit chain the way an operator's own tooling
// would: unlock the master key from the file the config names, derive the audit
// HMAC key through the vault, and read.
func offlineAuditKey(t *testing.T, cfg config.Config) []byte {
	t.Helper()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	masterKey, err := unlockMasterKey(context.Background(), cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(masterKey)
	secretVault, err := vault.New(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer secretVault.Close()
	auditKey, err := loadAuditHMACKey(store, secretVault, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	return auditKey
}
