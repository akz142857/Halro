//go:build linux

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	gatewaycore "github.com/akz142857/Halro/internal/gateway"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/requestmeta"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

func TestStandalonePhysicalTerminalActualENOSPCCompletesAndReopens(t *testing.T) {
	root := os.Getenv("HALRO_TERMINAL_ENOSPC_ROOT")
	if root == "" {
		t.Skip("requires explicit dedicated Linux filesystem <=512 MiB")
	}
	var fs unix.Statfs_t
	info, err := os.Lstat(root)
	if !filepath.IsAbs(root) || err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("unsafe fixture root", err)
	}
	expectedType := int64(unix.TMPFS_MAGIC)
	if os.Getenv("HALRO_TERMINAL_ENOSPC_FS") == "ext4" {
		expectedType = unix.EXT4_SUPER_MAGIC
	} else if os.Getenv("HALRO_TERMINAL_ENOSPC_FS") != "" {
		t.Fatal("unknown dedicated filesystem declaration")
	}
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != expectedType || uint64(fs.Blocks)*uint64(fs.Bsize) > 512<<20 {
		t.Fatal("refusing to exhaust filesystem outside explicit dedicated guard", fs, err)
	}
	directory, err := os.MkdirTemp(root, "halro-physical-terminal-")
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
	actualOS := os.Getenv("HALRO_TERMINAL_ACTUAL_OS_PRESSURE") == "1"
	if actualOS {
		pressure := physicalTerminalActualOSPressurePolicy()
		cfg = activateClosedRuntimeMemoryStateFixtureWithPressure(t, ctx, cfg, instance, false, &pressure)
	} else {
		cfg = activateClosedRuntimeMemoryStateFixture(t, ctx, cfg, instance, false)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("HALRO_TERMINAL_ENOSPC_DIAGNOSTIC") == "1" {
		logger = slog.New(slog.NewTextHandler(os.Stdout, nil))
	}
	opts := OpenOptions{AllowGeneratedSetupToken: true, resourceSampleSource: pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })}
	if actualOS {
		opts.resourceSampleSource = nil
	}
	r, err := OpenWithOptions(ctx, cfg, logger, opts)
	if err != nil {
		t.Fatal("physical selected Runtime", err)
	}
	defer r.Close()
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	r.stopBackgroundWork() // nonessential writers are paused; pressure owner remains live.
	fillerPath := filepath.Join(directory, "foreign-filler")
	defer os.Remove(fillerPath)
	var calls atomic.Int32
	var candidates [2]os.FileInfo
	var filled int64
	var beforeReturnErr error
	adapter := &selectedCompletionAdapter{contentCanaryAdapter: &contentCanaryAdapter{}, beforeReturn: func() (resultErr error) {
		defer func() { beforeReturnErr = resultErr }()
		if calls.Add(1) != 1 {
			return errors.New("upstream ran again after capacity exhaustion")
		}
		if debt := r.ledger.PhysicalTerminalDebt(); debt != 2 {
			return fmt.Errorf("native terminal vector not prepaid: %d", debt)
		}
		for i := range candidates {
			var err error
			candidates[i], err = os.Lstat(filepath.Join(cfg.Storage.DataDir, "memory-state", "ledger", fmt.Sprintf("terminal-root-%02d", i)))
			if err != nil || candidates[i].Size() != 0 {
				return fmt.Errorf("actual future root inode missing: %w", err)
			}
		}
		// Join each actual allocation. ext4 delayed allocation otherwise reports
		// a successful buffered Write whose later Sync is the ENOSPC boundary.
		filler, err := os.OpenFile(fillerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_SYNC, 0600)
		if err != nil {
			return err
		}
		chunk := make([]byte, 64<<10)
		for {
			n, writeErr := filler.Write(chunk)
			filled += int64(n)
			if writeErr != nil {
				if !errors.Is(writeErr, syscall.ENOSPC) {
					_ = filler.Close()
					return writeErr
				}
				if len(chunk) > int(fs.Bsize) {
					chunk = chunk[:fs.Bsize]
					continue
				}
				break
			}
			if filled > 512<<20 {
				_ = filler.Close()
				return errors.New("fixture exceeded mount guard")
			}
		}
		if err := errors.Join(filler.Sync(), filler.Close()); err != nil {
			return err
		}
		if err := unix.Statfs(root, &fs); err != nil || fs.Bavail != 0 {
			return fmt.Errorf("foreign inode did not exhaust actual blocks: %d %v", fs.Bavail, err)
		}
		if actualOS {
			deadline := time.Now().Add(5 * time.Second)
			for r.resourcePressure.monitor.Snapshot().State == resourcebudget.Healthy && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			observed := r.resourcePressure.monitor.Snapshot()
			t.Logf("actual OS pressure after foreign ENOSPC: %+v", observed)
			if observed.State == resourcebudget.Healthy {
				return errors.New("actual OS monitor did not observe exhausted target filesystem")
			}
		}
		before, _, _ := r.ledger.ChainHead()
		_, ordinaryErr := r.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, "req_forbidden_ordinary", "chat")
		after, _, _ := r.ledger.ChainHead()
		capacityRefusal := errors.Is(ordinaryErr, syscall.ENOSPC) || actualOS && errors.Is(ordinaryErr, resourcebudget.ErrPressure)
		if !capacityRefusal || before != after || r.ledger.PhysicalTerminalDebt() != 2 {
			return fmt.Errorf("ordinary append stole terminal capacity: %v %+v %+v", ordinaryErr, before, after)
		}
		if _, err := r.ledger.Roll(); !errors.Is(err, resourcebudget.ErrPhysicalAllocation) {
			return fmt.Errorf("Roll replaced admitted destination: %v", err)
		}
		return nil
	}}
	profile, _ := provider.BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	profiled, err := provider.NewLegacyAdapterBridge(adapter, profile, domain.EvidenceForCapabilities(domain.ProviderCapabilities{Chat: true}, domain.EvidenceDeclared))
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
	serve := func(id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"chat","messages":[{"role":"user","content":"hello"}],"max_tokens":10}`))
		req = req.WithContext(requestmeta.WithRequestID(ctx, id))
		req.Header.Set("Authorization", "Bearer "+credentials.GatewayKey)
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		if os.Getenv("HALRO_TERMINAL_INTERNAL_CAUSE_DIAGNOSTIC") == "1" {
			// Diagnostic only: retain the actual Service and its actual resource
			// middleware, and expose Cause solely to this test. The ordinary
			// path below remains the real HTTP acceptance fixture.
			r.requireResourcePressure(staleErrorOpenAI)(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var input openaiapi.ChatCompletionRequest
				if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
					t.Fatal(err)
				}
				output, err := r.gatewayService.Chat(requestmeta.WithInboundRequest(req.Context(), input), credentials.GatewayKey, input)
				if err != nil {
					var classified *gatewaycore.Error
					if errors.As(err, &classified) {
						t.Logf("internal Service diagnostic cause: %v", classified.Cause)
					}
					http.Error(w, err.Error(), http.StatusServiceUnavailable)
					return
				}
				_ = json.NewEncoder(w).Encode(output)
			})).ServeHTTP(response, req)
		} else {
			r.gatewayRouter().ServeHTTP(response, req)
		}
		return response
	}
	response := serve("req_physical_complete")
	if os.Getenv("HALRO_TERMINAL_INTERNAL_CAUSE_DIAGNOSTIC") == "1" {
		t.Fatalf("diagnostic scope only, not HTTP acceptance: status=%d body=%s", response.Code, response.Body.String())
	}
	requestID := response.Header().Get("X-Request-ID")
	if response.Code != http.StatusOK || calls.Load() != 1 || r.status.Load() != ledger.AccountingHealthy || r.ledger.PhysicalTerminalDebt() != 0 {
		t.Fatal("terminal failed at real ENOSPC", response.Code, response.Body.String(), r.status.Load(), r.ledger.PhysicalTerminalDebt(), calls.Load(), filled, beforeReturnErr, "metadata recovery", r.store.RecoveryError(), "budget", r.resourcePressure.logicalBudget().Snapshot())
	}
	currentRoot, err := os.Lstat(filepath.Join(cfg.Storage.DataDir, "memory-state", "ledger", "active.root"))
	if err != nil || !os.SameFile(candidates[1], currentRoot) {
		t.Fatal("publisher allocated a different root inode", err)
	}
	var q ledger.ProjectedRequest
	var found bool
	// This assertion borrows the actual global catch-up owner paid before
	// upstream. A fresh ordinary read is correctly refused by real pressure.
	if err := r.selectedState.generation.Owners.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		q, found, err = r.selectedState.generation.Owners.Ledger.ReadRequest(ctx, requestID)
		if err != nil || !found || q.Finalized == nil {
			return fmt.Errorf("actual selected terminal fact: found=%t finalized=%t: %w", found, q.Finalized != nil, err)
		}
		return nil
	}); err != nil {
		t.Fatal("actual selected root did not commit terminal fact", err)
	}
	before, _, _ := r.ledger.ChainHead()
	denied := serve("req_no_new_capacity")
	after, _, _ := r.ledger.ChainHead()
	if denied.Code != http.StatusServiceUnavailable || calls.Load() != 1 || before != after {
		t.Fatal("insufficient capacity reached upstream or native", denied.Code, denied.Body.String(), calls.Load(), before, after)
	}
	if actualOS {
		// The foreign writer still owns all free blocks and the OS monitor is
		// still pressured. Complete the real seven-root publication before
		// freeing anything or accepting a new ordinary operation.
		if err := r.CheckpointMemoryState(ctx); err != nil {
			t.Fatal("actual whole common publication at OS pressure/ENOSPC", err)
		}
	}
	if actualOS && os.Getenv("HALRO_TERMINAL_CLOSE_AT_ENOSPC") == "1" {
		// Common publication may retire unused tails. An independent writer
		// takes those blocks too, before the actual Runtime close begins.
		filler, err := os.OpenFile(fillerPath, os.O_WRONLY|os.O_APPEND|syscall.O_SYNC, 0600)
		if err != nil {
			t.Fatal(err)
		}
		t.Log("actual whole common publication completed with original targets at real OS pressure")
		chunk := make([]byte, 1<<20)
		size := len(chunk)
		for {
			n, writeErr := filler.Write(chunk[:size])
			filled += int64(n)
			if writeErr != nil {
				if !errors.Is(writeErr, syscall.ENOSPC) {
					t.Fatal(errors.Join(writeErr, filler.Close()))
				}
				if size > 4096 {
					size = 4096
					continue
				}
				break
			}
			if filled > 512<<20 {
				t.Fatal("foreign refill crossed explicit filesystem bound")
			}
		}
		if err := errors.Join(filler.Sync(), filler.Close()); err != nil {
			t.Fatal(err)
		}
		if err := unix.Statfs(root, &fs); err != nil || fs.Bavail != 0 {
			t.Fatal("close fixture has free blocks", fs.Bavail, err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for r.resourcePressure.monitor.Snapshot().State == resourcebudget.Healthy && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if r.resourcePressure.monitor.Snapshot().State == resourcebudget.Healthy {
			t.Fatal("real OS pressure missing before close")
		}
		beforeAudit := r.audit.Summary()
		_, metadataPath := memoryStateMetadataPaths(cfg, r.selectedState.bootstrap.MetadataSlot)
		beforeMetadata, err := os.Stat(metadataPath)
		if err != nil {
			t.Fatal(err)
		}
		beforeNative, beforeHash, beforeOK := r.ledger.ChainHead()
		if !beforeOK {
			t.Fatal("native head not authenticated before close")
		}
		if err := r.Close(); err != nil {
			t.Fatal("actual full-filesystem Runtime close", err)
		}
		afterMetadata, err := os.Stat(metadataPath)
		afterNative, afterHash, afterOK := r.ledger.ChainHead()
		if err != nil || !afterOK || !os.SameFile(beforeMetadata, afterMetadata) || beforeMetadata.Size() != afterMetadata.Size() || beforeAudit != r.audit.Summary() || beforeNative != afterNative || beforeHash != afterHash {
			t.Fatal("pressured Close appended a new shutdown fact or changed accepted source", err, afterOK)
		}
		t.Log("close-only scope: actual pressured Runtime close joined, no new audit/Metadata/Ledger fact; no Roll/repack/restart claim")
		return
	}
	if err := os.Remove(fillerPath); err != nil {
		t.Fatal(err)
	}
	if actualOS {
		deadline := time.Now().Add(5 * time.Second)
		for r.resourcePressure.monitor.Snapshot().State != resourcebudget.Healthy && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if sample := r.resourcePressure.monitor.Snapshot(); sample.State != resourcebudget.Healthy {
			t.Fatal("actual OS did not leave pressure after foreign inode retirement", sample)
		}
	}
	// Background catch-up was deliberately stopped for the original paid
	// terminal proof. Complete that actual backlog before admitting another
	// generation: eight 16MiB Usage targets cannot fit its declared 128MiB pack.
	// Use the real Collector and seven-root checkpoint, not a larger ceiling.
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal("original physical collector before successor admission", err)
	}
	if err := r.CheckpointMemoryState(ctx); err != nil {
		t.Fatal("original physical common checkpoint before successor admission", err)
	}
	previousNative, err := os.Stat(cfg.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	roll, err := r.ledger.Roll()
	if err != nil || !roll.Rolled {
		t.Fatal("physical native owner Roll", roll, err)
	}
	successorNative, err := os.Stat(cfg.LedgerPath())
	if err != nil || os.SameFile(previousNative, successorNative) {
		t.Fatal("Roll did not publish actual successor", err)
	}
	request, err := r.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, "req_physical_after_roll", "chat")
	if err != nil {
		t.Fatal("new ordinary admission on actual successor", err)
	}
	if err := r.accounting.Finalize(ctx, request, "success"); err != nil {
		t.Fatal("successor durable Apply", err)
	}
	if r.ledger.PhysicalTerminalDebt() != 0 {
		t.Fatal("Roll fabricated terminal debt retirement")
	}
	packPath := filepath.Join(cfg.Storage.DataDir, "memory-state", "ledger", "pages")
	oldPack, err := os.Stat(packPath)
	if err != nil {
		t.Fatal(err)
	}
	// Controlled accounting churn uses the same compound admission closure as
	// the real Gateway. Its large synthetic model tests COW capacity, whereas
	// the earlier canary HTTP request is the actual business ingress proof.
	admit := standalonePhysicalTerminalAdmission(cfg, r.selectedState)
	beginAndFinalize := func(id, model string) (resultErr error) {
		// Match the real HTTP reservation's finite original envelope, and enter
		// its terminal continuation only for Finalize. A second ordinary append
		// must never masquerade as an already-paid completion.
		p := r.resourcePressure
		work, err := p.budget.TryAcquireCompletableWork(resourcebudget.Active, resourcebudget.Cost{RAM: p.requestRAM}, resourcebudget.Cost{RAM: p.completionRAM, Disk: p.completionDisk}, p.completionWorkspace)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, work.Close(), work.Active.Release()) }()
		prepared, finish, err := admit(work.Bind(ctx), cfg.Gateway.MaxTotalAttempts+1)
		if err != nil {
			return fmt.Errorf("original compound physical admission: %w", err)
		}
		request, beginErr := r.accounting.BeginRequestDetailed(prepared, credentials.ProjectID, credentials.KeyID, id, model)
		if beginErr != nil {
			return errors.Join(fmt.Errorf("original compound accounting admission: %w", beginErr), finish())
		}
		finalizeErr := resourcebudget.WithCompletionWorkspace(prepared, func(ctx context.Context) error {
			return r.accounting.Finalize(ctx, request, "success")
		})
		if finalizeErr != nil {
			finalizeErr = fmt.Errorf("original compound finalization: %w", finalizeErr)
		}
		return errors.Join(finalizeErr, finish())
	}
	repacked := false
	// Large valid model identifiers grow real historical COW pages with few fsyncs.
	churnModel := strings.Repeat("m", 128<<10)
	for i := 0; i < 128; i++ {
		if err := beginAndFinalize(fmt.Sprintf("req_physical_repack_%d", i), churnModel); err != nil {
			t.Fatal("actual compound physical churn", i, err, "actual budget", r.resourcePressure.logicalBudget().Snapshot())
		}
		if err := r.CheckpointMemoryState(ctx); err != nil {
			t.Fatal("actual physical common checkpoint/repack", i, err)
		}
		current, statErr := os.Stat(packPath)
		if statErr == nil {
			paid, paidErr := r.selectedState.repackDisk.Cost()
			t.Log("actual completed churn pack", i, current.Size(), "repack reservation", paid, paidErr)
		}
		if errors.Is(statErr, os.ErrNotExist) || statErr == nil && !os.SameFile(oldPack, current) {
			repacked = true
			break
		}
		if statErr != nil {
			t.Fatal(statErr)
		}
	}
	if !repacked {
		t.Fatal("actual physical fixture never retired old COW pack")
	}
	if err := beginAndFinalize("req_physical_after_repack", "chat"); err != nil {
		t.Fatal("compound admission after physical common repack", err)
	}
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.drainAccountingBusiness(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal("actual owner join", err)
	}
	configPath := filepath.Join(directory, "fresh-process.json")
	configBytes, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, configBytes, 0600); err != nil {
		t.Fatal(err)
	}
	crash := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStandalonePhysicalPreparationSIGKILLHelper$", "-test.count=1")
	crash.Env = append(os.Environ(), "HALRO_PHYSICAL_PREPARATION_CHILD="+configPath, "HALRO_PHYSICAL_CRASH_REQUEST="+requestID)
	output, crashErr := crash.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(crashErr, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("post-repack actual preparation SIGKILL: %v %s", crashErr, output)
	}
	if _, err := os.Stat(filepath.Join(cfg.Storage.DataDir, "memory-state", "ledger", "terminal.physical.0")); err != nil {
		t.Fatal("post-repack receipt missing", err)
	}
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStandalonePhysicalPreparationSIGKILLHelper$", "-test.count=1")
	child.Env = append(os.Environ(), "HALRO_PHYSICAL_PREPARATION_CHILD="+configPath, "HALRO_PHYSICAL_VERIFY_REQUEST="+requestID)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("actual fresh process after common repack: %v %s", err, output)
	}
	again, err := OpenWithOptions(ctx, cfg, logger, opts)
	if err != nil {
		t.Fatal("actual selected reopen", err)
	}
	defer again.Close()
	if err := again.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	q, found, err = again.selectedState.generation.Owners.Ledger.ReadRequest(ctx, requestID)
	if err != nil || !found || q.Finalized == nil {
		t.Fatal("reopen lost completed terminal", q, found, err)
	}
	if err := again.drainAccountingBusiness(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("actual same-mount foreign ENOSPC filled=%d; prepaid root inode published, two terminal facts committed, ordinary/new upstream refused, Roll transferred actual native owner and new durable Apply succeeded, old COW pack retired by common checkpoint/repack, post-repack preparation SIGKILL and fresh-process receipt recovery succeeded", filled)
}

// This is an explicit real-OS acceptance profile for the private 2GiB ext4
// runner, separate from the deterministic logical-mechanism sample fixtures.
func physicalTerminalActualOSPressurePolicy() config.ResourcePressure {
	return config.ResourcePressure{RuntimeEnterBytes: 1 << 30, RuntimeExitBytes: 896 << 20, CgroupEnterBytes: 1792 << 20, CgroupExitBytes: 1536 << 20, HeadroomEnterBytes: 32 << 20, HeadroomExitBytes: 64 << 20, DiskEnterBytes: 4096, DiskExitBytes: 8192, InodesEnter: 128, InodesExit: 256, MaxAge: config.Duration(5 * time.Second), Cooldown: config.Duration(100 * time.Millisecond), SampleInterval: config.Duration(10 * time.Millisecond)}
}
