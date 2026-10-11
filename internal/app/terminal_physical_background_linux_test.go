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
	"slices"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/requestmeta"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

// backgroundWriterOutcome is what one maintenance writer did at real ENOSPC
// while an accepted request held its prepaid terminal reserve.
type backgroundWriterOutcome struct {
	name string
	err  error
}

// TestStandalonePhysicalBackgroundWritersAtENOSPC is the I-03 acceptance for
// the writers that share the service's filesystem with the terminal reserve
// (board §36). Each one runs while a request is accepted and its terminal is
// prepaid, on a filesystem a foreign writer has filled to real ENOSPC. Each
// must complete or defer cleanly — no recovery latch, no Ledger append, no
// prepaid debt retired, no own temporary left behind — and the accepted
// terminal must still complete. After the space returns every writer must
// succeed, the offline verifiers must pass, and a reopen must read the
// terminal.
//
// Writers out of scope, each for a reason in code rather than by choice:
// Ledger compaction is disabled while Usage is in disk mode
// (ledgerArchivedThrough), the selected Metadata facade refuses journal trim
// (ErrFacadeMaintenance), the failure-capture purge only removes files, and
// backup is an offline command.
func TestStandalonePhysicalBackgroundWritersAtENOSPC(t *testing.T) {
	root := os.Getenv("HALRO_TERMINAL_ENOSPC_ROOT")
	if root == "" || os.Getenv("HALRO_TERMINAL_ACTUAL_OS_PRESSURE") != "1" {
		t.Skip("requires explicit dedicated Linux filesystem <=512 MiB and the actual OS sampler")
	}
	var fs unix.Statfs_t
	info, err := os.Lstat(root)
	if !filepath.IsAbs(root) || err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("unsafe fixture root", err)
	}
	if os.Getenv("HALRO_TERMINAL_ENOSPC_FS") != "ext4" {
		t.Fatal("background writer acceptance needs the dedicated ext4 declaration")
	}
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC || uint64(fs.Blocks)*uint64(fs.Bsize) > 512<<20 {
		t.Fatal("refusing to exhaust filesystem outside explicit dedicated guard", fs, err)
	}
	directory, err := os.MkdirTemp(root, "halro-physical-background-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
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
	pressure := physicalTerminalActualOSPressurePolicy()
	cfg = activateClosedRuntimeMemoryStateFixtureWithPressure(t, ctx, cfg, instance, false, &pressure)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("HALRO_TERMINAL_ENOSPC_DIAGNOSTIC") == "1" {
		logger = slog.New(slog.NewTextHandler(os.Stdout, nil))
	}
	opts := OpenOptions{AllowGeneratedSetupToken: true}
	r, err := OpenWithOptions(ctx, cfg, logger, opts)
	if err != nil {
		t.Fatal("physical selected Runtime", err)
	}
	defer r.Close()
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	// The scheduled loop is stopped so that each writer runs exactly when the
	// test says; the writers themselves are the production functions.
	r.stopBackgroundWork()
	// Sealing is configured below its 16 MiB floor on purpose: the floor is a
	// production cadence, and this fixture needs the roll to be due now.
	r.config.Ledger.Seal = config.LedgerSeal{Enabled: true, MaxActiveBytes: 1, Compress: true}
	if r.usage == nil || r.usage.disk == nil {
		t.Fatal("selected Runtime is not in disk Usage mode; the compaction exclusion would not hold")
	}
	if _, err := r.store.TrimMetadataJournal(0); err == nil {
		t.Fatal("selected Metadata accepted a journal trim; the trim exclusion would not hold")
	}
	dataDir := cfg.Storage.DataDir
	exportRoot := cfg.GovernanceExportPath()
	exportAt := func(name string) error {
		_, err := r.writeGovernanceExport(ctx, filepath.Join(exportRoot, name))
		return err
	}
	writers := []struct {
		name string
		run  func() error
	}{
		{"usage-disk-cycle", func() error { return r.maintainUsageDiskCycle(ctx, time.Now()) }},
		{"ledger-seal", func() error {
			// sealLedgerGeneration logs rather than returns; its observable
			// contract is whether it rolled and whether it latched recovery.
			if rolled, err := sealRolled(cfg, r.sealLedgerGeneration); err != nil || rolled {
				return err
			}
			return errBackgroundWriterDeferred
		}},
		{"governance-export", func() error { return exportAt("gex_at_enospc") }},
	}
	// The common checkpoint joins every business ticket, so it cannot run on
	// the request's own goroutine. It starts from the hook on a goroutine of
	// its own, as the scheduled loop runs it, and finishes at ENOSPC after
	// the accepted terminal has released its ticket.
	checkpointDone := make(chan error, 1)
	var baseTemps []string

	fillerPath := filepath.Join(directory, "foreign-filler")
	defer os.Remove(fillerPath)
	var calls atomic.Int32
	var filled int64
	var outcomes []backgroundWriterOutcome
	var hookErr error
	// After space returns, an admitted request reaches upstream as usual; the
	// ENOSPC work below belongs to the first call alone.
	var spaceReturned atomic.Bool
	adapter := &selectedCompletionAdapter{contentCanaryAdapter: &contentCanaryAdapter{}, beforeReturn: func() (resultErr error) {
		if calls.Add(1) != 1 {
			if spaceReturned.Load() {
				return nil
			}
			hookErr = errors.New("upstream ran again after capacity exhaustion")
			return hookErr
		}
		defer func() { hookErr = resultErr }()
		if debt := r.ledger.PhysicalTerminalDebt(); debt != 2 {
			return fmt.Errorf("native terminal vector not prepaid: %d", debt)
		}
		n, err := fillForeignUntilENOSPC(fillerPath, int(fs.Bsize))
		filled += n
		if err != nil {
			return err
		}
		if err := waitBackgroundPressure(r, true); err != nil {
			return err
		}
		baseTemps = haPhysicalOwnedTemporaries(t, dataDir)
		for _, writer := range writers {
			if err := unix.Statfs(root, &fs); err != nil || fs.Bavail != 0 {
				// A writer that freed blocks (a retired tail, say) would let the
				// next one run off-pressure. Take them back first.
				n, err := fillForeignUntilENOSPC(fillerPath, int(fs.Bsize))
				filled += n
				if err != nil {
					return fmt.Errorf("refill before %s: %w", writer.name, err)
				}
			}
			headBefore, hashBefore, _ := r.ledger.ChainHead()
			runErr := writer.run()
			outcomes = append(outcomes, backgroundWriterOutcome{writer.name, runErr})
			t.Logf("BACKGROUND_AT_ENOSPC writer=%s err=%v", writer.name, runErr)
			headAfter, hashAfter, _ := r.ledger.ChainHead()
			if status := r.status.Load(); status != ledger.AccountingHealthy {
				return fmt.Errorf("%s at ENOSPC latched accounting status %v", writer.name, status)
			}
			if debt := r.ledger.PhysicalTerminalDebt(); debt != 2 {
				return fmt.Errorf("%s at ENOSPC retired prepaid terminal debt: %d", writer.name, debt)
			}
			if headBefore != headAfter || hashBefore != hashAfter {
				return fmt.Errorf("%s at ENOSPC appended to the Ledger: %+v -> %+v", writer.name, headBefore, headAfter)
			}
			if left := haPhysicalOwnedTemporaries(t, dataDir); !slices.Equal(left, baseTemps) {
				return fmt.Errorf("%s at ENOSPC left own temporaries: %v (before %v)", writer.name, left, baseTemps)
			}
			if writer.name == "governance-export" && runErr != nil {
				if _, err := os.Lstat(filepath.Join(exportRoot, "gex_at_enospc")); !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("failed governance export left its target: %v", err)
				}
			}
		}
		go func() {
			checkpointCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			checkpointDone <- r.CheckpointMemoryState(checkpointCtx)
		}()
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
		r.gatewayRouter().ServeHTTP(response, req)
		return response
	}

	response := serve("req_background_at_enospc")
	requestID := response.Header().Get("X-Request-ID")
	if hookErr != nil {
		t.Fatal("background writers at ENOSPC:", hookErr)
	}
	if len(outcomes) != len(writers) {
		t.Fatal("not every writer ran at ENOSPC", outcomes)
	}
	if response.Code != http.StatusOK || calls.Load() != 1 || r.status.Load() != ledger.AccountingHealthy || r.ledger.PhysicalTerminalDebt() != 0 {
		t.Fatal("accepted terminal failed after background writers at ENOSPC", response.Code, response.Body.String(), r.status.Load(), r.ledger.PhysicalTerminalDebt(), calls.Load(), filled)
	}
	checkpointErr := <-checkpointDone
	outcomes = append(outcomes, backgroundWriterOutcome{"common-checkpoint", checkpointErr})
	t.Logf("BACKGROUND_AT_ENOSPC writer=common-checkpoint err=%v", checkpointErr)
	if status := r.status.Load(); status != ledger.AccountingHealthy {
		t.Fatal("checkpoint at ENOSPC latched accounting status", status)
	}
	if left := haPhysicalOwnedTemporaries(t, dataDir); !slices.Equal(left, baseTemps) {
		t.Fatal("checkpoint at ENOSPC left own temporaries", left, baseTemps)
	}
	if err := r.selectedState.generation.Owners.Directory.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		q, found, err := r.selectedState.generation.Owners.Ledger.ReadRequest(ctx, requestID)
		if err != nil || !found || q.Finalized == nil {
			return fmt.Errorf("terminal fact: found=%t finalized=%t: %w", found, q.Finalized != nil, err)
		}
		return nil
	}); err != nil {
		t.Fatal("selected root did not commit the terminal", err)
	}
	if denied := serve("req_background_no_capacity"); denied.Code != http.StatusServiceUnavailable || calls.Load() != 1 {
		t.Fatal("new work reached upstream at ENOSPC", denied.Code, denied.Body.String())
	}

	// Space returns. Every writer must now do its work.
	if err := os.Remove(fillerPath); err != nil {
		t.Fatal(err)
	}
	spaceReturned.Store(true)
	if err := waitBackgroundPressure(r, false); err != nil {
		t.Fatal(err)
	}
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal("collector after space returned", err)
	}
	if err := r.CheckpointMemoryState(ctx); err != nil {
		t.Fatal("checkpoint after space returned", err)
	}
	if rolled, err := sealRolled(cfg, r.sealLedgerGeneration); err != nil || !rolled || r.status.Load() != ledger.AccountingHealthy {
		t.Fatal("seal after space returned did not roll a verified generation", rolled, err, r.status.Load())
	}
	if err := r.maintainUsageDiskCycle(ctx, time.Now()); err != nil {
		t.Fatal("usage disk cycle after space returned", err)
	}
	// A selected install has no Governance export source yet (board §36 v7:
	// only the bbolt Ledger projection serves one, and the legacy fallback
	// needs the old State). It must refuse cleanly, not half-publish.
	if err := exportAt("gex_after_space"); !errors.Is(err, ledger.ErrGovernanceExportBackend) || r.status.Load() != ledger.AccountingHealthy {
		t.Fatal("selected Governance export after space returned did not refuse cleanly", err, r.status.Load())
	}
	if _, err := os.Lstat(filepath.Join(exportRoot, "gex_after_space")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused Governance export left a target", err)
	}
	if err := r.CheckpointMemoryState(ctx); err != nil {
		t.Fatal("final checkpoint after space returned", err)
	}
	after := serve("req_background_after_space")
	if after.Code != http.StatusOK || calls.Load() != 2 || after.Header().Get("X-Request-ID") == "" {
		t.Fatal("admission after space returned", after.Code, after.Body.String())
	}
	afterID := after.Header().Get("X-Request-ID")
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.drainAccountingBusiness(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal("close after background writers", err)
	}
	if left := haPhysicalOwnedTemporaries(t, dataDir); len(left) != 0 {
		t.Fatal("own temporaries after close", left)
	}
	again, err := OpenWithOptions(ctx, cfg, logger, opts)
	if err != nil {
		t.Fatal("reopen", err)
	}
	defer again.Close()
	if err := again.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{requestID, afterID} {
		q, found, err := again.selectedState.generation.Owners.Ledger.ReadRequest(ctx, id)
		if err != nil || !found || q.Finalized == nil {
			t.Fatal("reopen lost a terminal", id, found, err)
		}
	}
	if err := again.drainAccountingBusiness(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	summary := ""
	for _, o := range outcomes {
		summary += fmt.Sprintf(" %s=%v", o.name, o.err)
	}
	t.Logf("BACKGROUND_SUMMARY filled=%d%s", filled, summary)
}

var errBackgroundWriterDeferred = errors.New("writer deferred without rolling")

// sealRolled reports whether seal replaced the active WAL inode.
func sealRolled(cfg config.Config, seal func()) (bool, error) {
	before, err := os.Stat(cfg.LedgerPath())
	if err != nil {
		return false, err
	}
	seal()
	after, err := os.Stat(cfg.LedgerPath())
	if err != nil {
		return false, err
	}
	return !os.SameFile(before, after), nil
}

// fillForeignUntilENOSPC appends synchronous chunks to path until the
// filesystem refuses one block. O_SYNC joins each allocation: ext4 delayed
// allocation otherwise reports a buffered Write whose Sync is the boundary.
func fillForeignUntilENOSPC(path string, block int) (int64, error) {
	filler, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_SYNC, 0600)
	if err != nil {
		return 0, err
	}
	var filled int64
	chunk := make([]byte, 64<<10)
	for {
		n, writeErr := filler.Write(chunk)
		filled += int64(n)
		if writeErr != nil {
			if !errors.Is(writeErr, syscall.ENOSPC) {
				return filled, errors.Join(writeErr, filler.Close())
			}
			if len(chunk) > block {
				chunk = chunk[:block]
				continue
			}
			break
		}
		if filled > 512<<20 {
			return filled, errors.Join(errors.New("fixture exceeded mount guard"), filler.Close())
		}
	}
	return filled, errors.Join(filler.Sync(), filler.Close())
}

func waitBackgroundPressure(r *Runtime, pressured bool) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if (r.resourcePressure.monitor.Snapshot().State != resourcebudget.Healthy) == pressured {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("actual OS monitor did not reach pressured=%t: %+v", pressured, r.resourcePressure.monitor.Snapshot())
}
