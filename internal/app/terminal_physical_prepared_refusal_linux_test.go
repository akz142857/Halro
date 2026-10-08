//go:build linux

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	gatewaycore "github.com/akz142857/Halro/internal/gateway"
	"github.com/akz142857/Halro/internal/gatewayapi"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/requestmeta"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"golang.org/x/sys/unix"
)

// Only the foreign filesystem writer is injected. Both pin transitions and the
// budget refusal execute the original selected Metadata and Ledger owners.
type physicalRefusalPrices struct {
	*boltstore.Store
	runtime           *Runtime
	filler            string
	prepared, deleted int
	intent            domain.PricePinIntent
}

func (p *physicalRefusalPrices) PrepareDeploymentPricePin(ctx context.Context, deployment, attempt string, at time.Time, rollback, forward time.Duration) (domain.DeploymentPriceVersion, domain.PriceSnapshot, domain.PricePinIntent, error) {
	price, snapshot, intent, err := p.Store.PrepareDeploymentPricePin(ctx, deployment, attempt, at, rollback, forward)
	if err == nil {
		p.prepared++
		p.intent = intent
	}
	return price, snapshot, intent, err
}

func (p *physicalRefusalPrices) DeletePreparedDeploymentPricePin(ctx context.Context, attempt string) error {
	if p.prepared != 1 || attempt != p.intent.AttemptID || ctx.Err() != nil {
		return errors.New("original Prepared cleanup context missing")
	}
	if err := exhaustPhysicalAcceptedFixture(p.filler); err != nil {
		return err
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(p.filler, &fs); err != nil || fs.Bavail != 0 {
		return fmt.Errorf("foreign fill did not reach real ENOSPC: %w", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for p.runtime.resourcePressure.monitor.Snapshot().State != resourcebudget.Pressured {
		if time.Now().After(deadline) {
			return errors.New("actual OS pressure was not observed")
		}
		time.Sleep(time.Millisecond)
	}
	manager := p.runtime.resourcePressure.logicalBudget()
	before := manager.Snapshot()
	if err := p.Store.DeletePreparedDeploymentPricePin(ctx, attempt); err != nil {
		return err
	}
	after := manager.Snapshot()
	if before.Used != after.Used || before.NormalClaims != after.NormalClaims || before.RecoveryClaims != after.RecoveryClaims {
		return errors.New("actual pin cleanup acquired fresh budget at ENOSPC")
	}
	// Publishing the original COW target can retire its old pages. The
	// admission proof is Bavail=0 before deletion and unchanged paid ownership,
	// rather than requiring known retirement to keep every old block allocated.
	p.deleted++
	return nil
}

func TestStandalonePhysicalPreparedActualBudgetRefusalCleanupAtENOSPC(t *testing.T) {
	root := os.Getenv("HALRO_TERMINAL_ENOSPC_ROOT")
	if root == "" {
		t.Skip("requires explicitly isolated ext4 runner")
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC || uint64(fs.Blocks)*uint64(fs.Bsize) > 512<<20 {
		t.Fatal("refusing foreign filesystem", err)
	}
	dir, err := os.MkdirTemp(root, "prepared-refusal-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cfg := testConfig(t)
	cfg.Storage.DataDir, cfg.Storage.MasterKey.File = filepath.Join(dir, "data"), filepath.Join(dir, "master.key")
	cfg.Gateway.MaxTotalAttempts = 1
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	credentials, err := Bootstrap(ctx, cfg, BootstrapOptions{ProviderName: "refusal", ProviderType: domain.ProviderOpenAI, ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test", PublicModel: "chat", ProjectName: "refusal", DailyBudgetMicrosUSD: 1, InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000}, []byte("fixture-provider-key"))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	legacy, err := Open(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	instance := legacy.instanceID
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	limits := physicalTerminalBudgetFixture()
	limits.LedgerProjectionDiskBytes = 56 << 20
	limits.PhysicalTerminalReserve = true
	cfg.ResourceBudget = &limits
	pressure := physicalTerminalActualOSPressurePolicy()
	cfg = activateClosedRuntimeMemoryStateFixtureWithPressure(t, ctx, cfg, instance, false, &pressure)
	r, err := OpenWithOptions(ctx, cfg, logger, OpenOptions{AllowGeneratedSetupToken: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	r.stopBackgroundWork()
	prices := &physicalRefusalPrices{Store: r.store, runtime: r, filler: filepath.Join(dir, "foreign-fill")}
	defer os.Remove(prices.filler)
	var calls atomic.Int32
	adapter := &selectedCompletionAdapter{contentCanaryAdapter: &contentCanaryAdapter{}, beforeReturn: func() error { calls.Add(1); return nil }}
	profile, _ := provider.BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	bridge, err := provider.NewLegacyAdapterBridge(adapter, profile, domain.EvidenceForCapabilities(domain.ProviderCapabilities{Chat: true}, domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry()
	if err := registry.Register(provider.Target{ID: credentials.RouteID, DeploymentID: credentials.DeploymentID, ProviderID: credentials.ProviderID, PublicModel: "chat", ProviderModel: "gpt-test", Adapter: bridge, ProfileID: profile.ID, AccessSurface: profile.AccessSurface, Capabilities: provider.Capabilities{Chat: true}}); err != nil {
		t.Fatal(err)
	}
	registry.SetEligibility(r.routes)
	for _, old := range r.providers.Replace(registry) {
		old.Close()
	}
	p := r.resourcePressure
	r.gatewayService, err = gatewaycore.NewServiceWithOptions(r.auth, r.providers, r.accounting, gatewaycore.ServiceOptions{MaxAttempts: 1, TerminalAdmission: standalonePhysicalTerminalAdmission(cfg, r.selectedState), RouteGate: r.routes, TokenGuard: r.tokenGuard, Redactor: r.redactor, ResourceBudget: p.logicalBudget(), DeferredRAMBytes: p.deferredEnvelope(), CompletionRAMBytes: p.completionRAMEnvelope(), CompletionDiskBytes: p.completionDiskEnvelope(), CompletionWorkspace: p.completionProfile(), CaptureRAMBytes: p.captureEnvelope(), Pricing: prices, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	r.gateway, err = gatewayapi.NewWithOptions(r.gatewayService, gatewayapi.Options{MaxRequestBytes: cfg.Server.MaxRequestBytes, RouteTimeout: cfg.Gateway.RouteTotalTimeout.Value(), StreamTimeout: cfg.Gateway.StreamMaxDuration.Value(), WriteTimeout: cfg.Gateway.DownstreamWriteTimeout.Value(), AuthorizeKey: func(key string) (int64, error) {
		principal, err := r.auth.Authenticate(key, time.Now())
		if err != nil {
			return 0, err
		}
		return principal.Project.MaxRequestBytes, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"chat","messages":[{"role":"user","content":"hello"}],"max_tokens":10}`))
	req = req.WithContext(requestmeta.WithRequestID(ctx, "req_actual_prepared_refusal"))
	req.Header.Set("Authorization", "Bearer "+credentials.GatewayKey)
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	r.gatewayRouter().ServeHTTP(response, req)
	if !strings.Contains(response.Body.String(), "budget_exceeded") || calls.Load() != 0 || prices.prepared != 1 || prices.deleted != 1 {
		t.Fatal("actual refusal did not finish original pin cleanup", response.Code, response.Body.String(), calls.Load(), prices.prepared, prices.deleted)
	}
	if err := r.selectedState.generation.Owners.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		request, found, err := r.selectedState.generation.Owners.Ledger.ReadRequest(ctx, response.Header().Get("X-Request-ID"))
		if err != nil || !found || request.Finalized == nil {
			return fmt.Errorf("actual rejected request did not finalize: found=%t: %w", found, err)
		}
		return r.selectedState.generation.Owners.Metadata.View(ctx, func(tx *metadatastate.Transaction) error {
			_, found, err := tx.Get([]string{"deployment_price_pin_intents"}, []byte(prices.intent.AttemptID))
			if found {
				return errors.New("actual Prepared row survived cleanup")
			}
			return err
		})
	}); err != nil {
		t.Fatal("actual pin root", err)
	}
	if r.ledger.PhysicalTerminalDebt() != 0 {
		t.Fatal("refused request left its original native window live", r.ledger.PhysicalTerminalDebt())
	}
	if err := os.Remove(prices.filler); err != nil {
		t.Fatal(err)
	}
	waitRuntimePressure(t, p, resourcebudget.Healthy)
	if err := r.CheckpointMemoryState(ctx); err != nil {
		t.Fatal("actual common checkpoint after refusal", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal("actual original owner join", err)
	}
}
