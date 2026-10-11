//go:build linux

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/requestmeta"
	"golang.org/x/sys/unix"
)

// TestStandalonePhysicalRollAlignsProjection covers board §36's defect: with
// the physical terminal reserve, a Ledger Roll followed by a checkpoint or a
// Close before the next request retired the Ledger projection, and the
// instance could not reopen. A Roll now records the successor's head in the
// projection itself, so each of these orders must work, including a process
// killed right after the Roll returns.
func TestStandalonePhysicalRollAlignsProjection(t *testing.T) {
	root := os.Getenv("HALRO_TERMINAL_ENOSPC_ROOT")
	if root == "" {
		t.Skip("requires explicit dedicated Linux filesystem <=512 MiB")
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC || uint64(fs.Blocks)*uint64(fs.Bsize) > 512<<20 {
		t.Fatal("refusing a filesystem outside the explicit dedicated guard", fs, err)
	}
	directory, err := os.MkdirTemp(root, "halro-physical-roll-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	legacy, credentials, _ := openBootstrappedRuntimeShaped(t, "gpt-test", func(c *config.Config) {
		c.Storage.DataDir = filepath.Join(directory, "data")
		c.Storage.MasterKey.File = filepath.Join(directory, "master.key")
		c.Gateway.MaxTotalAttempts = 1
	})
	cfg, instance := legacy.config, legacy.instanceID
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	limits := physicalTerminalBudgetFixture()
	limits.LedgerProjectionDiskBytes = 56 << 20
	limits.PhysicalTerminalReserve = true
	cfg.ResourceBudget = &limits
	// Every process here, the offline verifier included, samples the real
	// dedicated filesystem; these are its thresholds.
	pressure := physicalTerminalActualOSPressurePolicy()
	cfg = activateClosedRuntimeMemoryStateFixtureWithPressure(t, ctx, cfg, instance, false, &pressure)
	// Sealing below its 16 MiB floor makes each Roll due on demand; the roll
	// itself is the production sealLedgerGeneration path.
	cfg.Ledger.Seal = config.LedgerSeal{Enabled: true, MaxActiveBytes: config.MinLedgerSealMaxActiveBytes, Compress: true}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := OpenOptions{AllowGeneratedSetupToken: true}
	open := func() *Runtime {
		t.Helper()
		r, err := OpenWithOptions(ctx, cfg, logger, opts)
		if err != nil {
			t.Fatal("open physical selected Runtime", err)
		}
		if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
			t.Fatal(err)
		}
		r.stopBackgroundWork()
		r.config.Ledger.Seal.MaxActiveBytes = 1
		installPhysicalRollCanary(t, r, credentials)
		return r
	}
	roll := func(r *Runtime) {
		t.Helper()
		rolled, err := sealRolled(cfg, r.sealLedgerGeneration)
		if err != nil || !rolled || r.status.Load() != ledger.AccountingHealthy {
			t.Fatal("seal did not roll a healthy generation", rolled, err, r.status.Load())
		}
	}
	finalized := func(r *Runtime, ids ...string) {
		t.Helper()
		for _, id := range ids {
			q, found, err := r.selectedState.generation.Owners.Ledger.ReadRequest(ctx, id)
			if err != nil || !found || q.Finalized == nil {
				t.Fatal("terminal not readable", id, found, err)
			}
		}
	}
	closeRuntime := func(r *Runtime) {
		t.Helper()
		if err := r.drainAccountingBusiness(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if err := r.Close(); err != nil {
			t.Fatal("close", err)
		}
	}

	r := open()
	a := servePhysicalRollChat(t, ctx, r, credentials, "req_roll_a")
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.CheckpointMemoryState(ctx); err != nil {
		t.Fatal(err)
	}
	// Roll, then checkpoint before any request.
	roll(r)
	if err := r.CheckpointMemoryState(ctx); err != nil {
		t.Fatal("checkpoint after Roll", err)
	}
	b := servePhysicalRollChat(t, ctx, r, credentials, "req_roll_b")
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.CheckpointMemoryState(ctx); err != nil {
		t.Fatal(err)
	}
	// Roll, then Close before any request.
	roll(r)
	closeRuntime(r)

	r = open()
	finalized(r, a, b)
	c := servePhysicalRollChat(t, ctx, r, credentials, "req_roll_c")
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.CheckpointMemoryState(ctx); err != nil {
		t.Fatal(err)
	}
	closeRuntime(r)

	// Roll in a fresh process, then SIGKILL it as soon as the Roll returns.
	configPath := filepath.Join(directory, "roll-child.json")
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStandalonePhysicalRollSIGKILLHelper$", "-test.count=1")
	child.Env = append(os.Environ(), "HALRO_PHYSICAL_ROLL_CHILD="+configPath)
	output, childErr := child.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(childErr, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("Roll child was not killed after rolling: %v %s", childErr, output)
	}

	r = open()
	finalized(r, a, b, c)
	finalized(r, servePhysicalRollChat(t, ctx, r, credentials, "req_roll_d"))
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.CheckpointMemoryState(ctx); err != nil {
		t.Fatal("checkpoint after a killed Roll", err)
	}
	closeRuntime(r)
}

// TestStandalonePhysicalRollSIGKILLHelper is the fresh process for the test
// above: it rolls the active generation and is killed as soon as Roll returns.
func TestStandalonePhysicalRollSIGKILLHelper(t *testing.T) {
	path := os.Getenv("HALRO_PHYSICAL_ROLL_CHILD")
	if path == "" {
		t.Skip("subprocess helper")
	}
	var cfg config.Config
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	r.stopBackgroundWork()
	result, err := r.ledger.Roll()
	if err != nil || !result.Rolled {
		t.Fatal("child Roll", result, err)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {}
}

func installPhysicalRollCanary(t *testing.T, r *Runtime, credentials BootstrapResult) {
	t.Helper()
	profile, _ := provider.BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	profiled, err := provider.NewLegacyAdapterBridge(&contentCanaryAdapter{}, profile, domain.EvidenceForCapabilities(domain.ProviderCapabilities{Chat: true}, domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry()
	if err := registry.Register(provider.Target{ID: credentials.RouteID, DeploymentID: credentials.DeploymentID, ProviderID: credentials.ProviderID, PublicModel: "chat", ProviderModel: "canary-model", Adapter: profiled, ProfileID: profile.ID, AccessSurface: profile.AccessSurface, Capabilities: provider.Capabilities{Chat: true}}); err != nil {
		t.Fatal(err)
	}
	for _, retired := range r.providers.Replace(registry) {
		retired.Close()
	}
}

func servePhysicalRollChat(t *testing.T, ctx context.Context, r *Runtime, credentials BootstrapResult, id string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"chat","messages":[{"role":"user","content":"hello"}],"max_tokens":10}`))
	req = req.WithContext(requestmeta.WithRequestID(ctx, id))
	req.Header.Set("Authorization", "Bearer "+credentials.GatewayKey)
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	r.gatewayRouter().ServeHTTP(response, req)
	if response.Code != http.StatusOK || response.Header().Get("X-Request-ID") == "" {
		t.Fatal("physical Roll fixture request", id, response.Code, response.Body.String())
	}
	return response.Header().Get("X-Request-ID")
}
