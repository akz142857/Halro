package app

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/akz142857/Heimdall/internal/tokenguard"
)

func TestRuntimeRejectsMissingTokenGuardPolicyReference(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.PutProject(context.Background(), domain.Project{
		ID: "project_1", Name: "Project", Enabled: true, TokenGuardPolicyID: "missing",
	}, 0)
	if closeErr := store.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("runtime accepted a missing token guard policy")
	}
}

func TestRuntimeLoadsReferencedTokenGuardPolicy(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	policy := domain.TokenGuardPolicy{
		ID: "guard_1", Name: "Guard", Enabled: true, Action: "temporary_block",
		RequestTokens: 100, MinimumSamples: 2, ViolationsBeforeBlock: 2,
		BlockTTL: time.Minute, Cooldown: 30 * time.Second,
	}
	if _, err := store.PutTokenGuardPolicy(context.Background(), policy, 0); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := store.PutProject(context.Background(), domain.Project{
		ID: "project_1", Name: "Project", Enabled: true, TokenGuardPolicyID: policy.ID,
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
	if !runtime.tokenGuard.HasPolicy(policy.ID) {
		t.Fatal("token guard policy was not loaded")
	}
}

func TestRuntimePersistsAndRestoresEWMABaseline(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	policy, err := store.PutTokenGuardPolicy(context.Background(), domain.TokenGuardPolicy{
		ID: "guard_ewma", Name: "EWMA", Enabled: true, Action: "observe",
		EWMAEnabled: true, EWMAAlpha: 0.5, EWMAMultiplier: 2,
		EWMAMinimumSamples: 10, EWMAWarmup: 10 * time.Second,
		EWMAEvaluationWindow: 10 * time.Second, EWMACooldown: time.Minute,
		EWMAAbsoluteRPM: 100,
	}, 0)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := store.PutProject(context.Background(), domain.Project{
		ID: "project_1", Name: "Project", Enabled: true, TokenGuardPolicyID: policy.ID,
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
	start := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	for index := 0; index <= 10; index++ {
		runtime.tokenGuard.Admit(tokenguard.Input{
			PolicyID: policy.ID, ProjectID: "project_1", KeyID: "key_1",
			EstimatedTokens: 1, Now: start.Add(time.Duration(index) * time.Second),
		})
	}
	runtime.saveTokenGuardCheckpoint()
	expected, err := runtime.tokenGuard.MarshalCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	actual, err := reopened.tokenGuard.MarshalCheckpoint()
	if err != nil || string(actual) != string(expected) {
		t.Fatalf("restored baseline mismatch err=%v\nexpected=%s\nactual=%s", err, expected, actual)
	}
}

func TestCorruptEWMACheckpointFallsBackToFixedLimits(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutTokenGuardCheckpoint([]byte(`{"version":1,"subjects":[{"policy_id":"broken"}]}`)); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("corrupt detect-only state prevented startup: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	payload, err := store.TokenGuardCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := tokenguard.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RestoreCheckpoint(payload); err != nil {
		t.Fatalf("corrupt checkpoint was not replaced by safe state: %v", err)
	}
}
