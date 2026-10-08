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

const physicalAcceptedRequest = "req_physical_accepted_sigkill"

type physicalAcceptedFixture struct {
	Config      config.Config
	Credentials BootstrapResult
	Filler      string
	RequestFile string
}

func TestStandalonePhysicalAcceptedSIGKILLHelper(t *testing.T) {
	path := os.Getenv("HALRO_PHYSICAL_ACCEPTED_CHILD")
	if path == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture physicalAcceptedFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	mode := os.Getenv("HALRO_PHYSICAL_ACCEPTED_MODE")
	if mode == "recover" {
		var fs unix.Statfs_t
		if err := unix.Statfs(filepath.Dir(fixture.Filler), &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC || fs.Bavail != 0 {
			t.Fatal("recovery did not start on the original full ext4", fs, err)
		}
		info, err := os.Stat(fixture.Filler)
		if err != nil || info.Size() <= 0 {
			t.Fatal("foreign filler disappeared", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	r, err := OpenWithOptions(ctx, fixture.Config, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{resourceSampleSource: pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })})
	if mode == "reject" {
		if err == nil || r != nil {
			t.Fatal("pending accepted work used NoDebt opaque cleanup", err)
		}
		t.Log("actual pending-without-authority startup refused:", err)
		return
	}
	if err != nil {
		t.Fatal("actual independent selected Runtime", err)
	}
	defer r.Close()
	r.stopBackgroundWork()
	if mode == "recover" {
		requestBytes, err := os.ReadFile(fixture.RequestFile)
		if err != nil {
			t.Fatal(err)
		}
		recovery := r.selectedState.services.Recovery
		if recovery.AttemptsSettled != 1 || recovery.RequestsFinalized != 1 {
			t.Fatal("actual accepted two-level recovery was bypassed", recovery)
		}
		request, found, err := r.selectedState.generation.Owners.Ledger.ReadRequest(ctx, string(requestBytes))
		if err != nil || !found || request.Finalized == nil {
			t.Fatal("original request did not terminate", request, found, err)
		}
		if r.status.Load() != ledger.AccountingHealthy || r.ledger.PhysicalTerminalDebt() != 0 || r.ledgerAppendDisk.physicalUnused != 0 {
			t.Fatal("restored native owner did not join", r.status.Load(), r.ledger.PhysicalTerminalDebt(), r.ledgerAppendDisk.physicalUnused)
		}
		current, found, err := r.selectedState.publisher.Current()
		if err != nil || !found || current != r.selectedState.generation.Seal || current.Positions[0].Sequence != recovery.ConfirmedHead.Sequence {
			t.Fatal("terminal cut lacks actual common publication", current, recovery, err)
		}
		if _, err := os.Stat(fixture.Filler); err != nil {
			t.Fatal("recovery deleted foreign data", err)
		}
		if err := os.Remove(fixture.Filler); err != nil {
			t.Fatal(err)
		}
		window, err := r.ledger.ReservePhysicalTerminals(ctx, 2)
		if err != nil {
			t.Fatal("Ready did not bind fresh preparation to recovered common root", err)
		}
		if err := window.Close(); err != nil {
			t.Fatal(err)
		}
		requestID, err := r.accounting.BeginRequestDetailed(ctx, fixture.Credentials.ProjectID, fixture.Credentials.KeyID, "req_after_accepted_recovery", "chat")
		if err != nil {
			t.Fatal(err)
		}
		if err := r.accounting.Finalize(ctx, requestID, "success"); err != nil {
			t.Fatal(err)
		}
		if err := r.drainAccountingBusiness(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if err := r.Close(); err != nil {
			t.Fatal("recovered owner actual Close", err)
		}
		return
	}
	if mode != "crash" {
		t.Fatal("unknown subprocess mode")
	}
	adapter := &selectedCompletionAdapter{contentCanaryAdapter: &contentCanaryAdapter{}, beforeReturn: func() (resultErr error) {
		defer func() {
			if resultErr != nil {
				t.Log("actual upstream crash boundary:", resultErr)
			}
		}()
		var requestID string
		if err := r.selectedState.generation.Owners.Ledger.WithRecoveryView(ctx, func(v ledger.AuthenticatedRecoveryView) error {
			requests, err := v.UnfinalizedRequestPage(ctx, "", ledger.ProjectionPageLimits{Rows: 2, Bytes: 1 << 20})
			if err != nil {
				return err
			}
			pending, err := v.PendingLeasePage(ctx, "", ledger.ProjectionPageLimits{Rows: 2, Bytes: 1 << 20})
			if err != nil {
				return err
			}
			if len(requests.Items) != 1 || len(pending.Items) != 1 || !requests.Done || !pending.Done || r.ledger.PhysicalTerminalDebt() != 2 {
				return errors.New("actual accepted request and original pending attempt were not captured")
			}
			requestID = requests.Items[0].Accepted.Event.RequestID
			return v.Complete(ctx)
		}); err != nil {
			return err
		}
		f, err := os.OpenFile(fixture.RequestFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, writeErr := f.WriteString(requestID)
		if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
			return err
		}
		if err := exhaustPhysicalAcceptedFixture(fixture.Filler); err != nil {
			return err
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
			return err
		}
		return errors.New("SIGKILL returned")
	}}
	profile, _ := provider.BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	profiled, err := provider.NewLegacyAdapterBridge(adapter, profile, domain.EvidenceForCapabilities(domain.ProviderCapabilities{Chat: true}, domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry()
	c := fixture.Credentials
	if err := registry.Register(provider.Target{ID: c.RouteID, DeploymentID: c.DeploymentID, ProviderID: c.ProviderID, PublicModel: "chat", ProviderModel: "canary-model", Adapter: profiled, ProfileID: profile.ID, AccessSurface: profile.AccessSurface, Capabilities: provider.Capabilities{Chat: true}}); err != nil {
		t.Fatal(err)
	}
	for _, retired := range r.providers.Replace(registry) {
		retired.Close()
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"chat","messages":[{"role":"user","content":"hello"}],"max_tokens":10}`))
	req = req.WithContext(requestmeta.WithRequestID(ctx, physicalAcceptedRequest))
	req.Header.Set("Authorization", "Bearer "+c.GatewayKey)
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	r.gatewayRouter().ServeHTTP(response, req)
	t.Fatal("accepted upstream did not reach SIGKILL", response.Code, response.Body.String())
}

func exhaustPhysicalAcceptedFixture(path string) error {
	var fs unix.Statfs_t
	if err := unix.Statfs(filepath.Dir(path), &fs); err != nil {
		return err
	}
	if fs.Type != unix.EXT4_SUPER_MAGIC || fs.Bsize <= 0 || uint64(fs.Blocks)*uint64(fs.Bsize) > 512<<20 {
		return errors.New("refusing to fill unqualified fixture")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_SYNC, 0600)
	if err != nil {
		return err
	}
	chunk := make([]byte, 64<<10)
	var filled int64
	for {
		n, err := f.Write(chunk)
		filled += int64(n)
		if filled > 512<<20 {
			return errors.Join(errors.New("fixture allocation bound exceeded"), f.Close())
		}
		if err != nil {
			if !errors.Is(err, syscall.ENOSPC) {
				return errors.Join(err, f.Close())
			}
			if int64(len(chunk)) > fs.Bsize {
				chunk = chunk[:fs.Bsize]
				continue
			}
			break
		}
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	if err := unix.Statfs(filepath.Dir(path), &fs); err != nil || fs.Bavail != 0 {
		return fmt.Errorf("actual ENOSPC was not proved: %d %v", fs.Bavail, err)
	}
	fmt.Printf("original accepted request at actual ext4 ENOSPC: foreign_bytes=%d bavail=%d\n", filled, fs.Bavail)
	return nil
}

func TestStandalonePhysicalAcceptedENOSPCSIGKILLRecoversBeforeReady(t *testing.T) {
	root := os.Getenv("HALRO_TERMINAL_ENOSPC_ROOT")
	if root == "" || os.Getenv("HALRO_TERMINAL_ENOSPC_FS") != "ext4" {
		t.Skip("requires dedicated ext4 <=512MiB")
	}
	var fs unix.Statfs_t
	if !filepath.IsAbs(root) {
		t.Fatal("unsafe fixture root")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("unsafe fixture", err)
	}
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC || uint64(fs.Blocks)*uint64(fs.Bsize) > 512<<20 {
		t.Fatal("unsafe filesystem", fs, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	directory := t.TempDir()
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
	cfg = activateClosedRuntimeMemoryStateFixture(t, ctx, cfg, instance, false)
	fixture := physicalAcceptedFixture{Config: cfg, Credentials: credentials, Filler: filepath.Join(directory, "foreign-filler"), RequestFile: filepath.Join(directory, "request-id")}
	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "fixture.json")
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStandalonePhysicalAcceptedSIGKILLHelper$", "-test.count=1", "-test.v")
	child.Env = append(os.Environ(), "HALRO_PHYSICAL_ACCEPTED_CHILD="+configPath, "HALRO_PHYSICAL_ACCEPTED_MODE=crash")
	output, err := child.CombinedOutput()
	t.Logf("accepted crash child: %s", output)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("did not reach accepted SIGKILL: %v %s", err, output)
	}
	recovery := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStandalonePhysicalAcceptedSIGKILLHelper$", "-test.count=1", "-test.v")
	recovery.Env = append(os.Environ(), "HALRO_PHYSICAL_ACCEPTED_CHILD="+configPath, "HALRO_PHYSICAL_ACCEPTED_MODE=recover")
	output, err = recovery.CombinedOutput()
	t.Logf("independent recovery child: %s", output)
	if err != nil {
		t.Fatalf("full ext4 accepted recovery failed: %v %s", err, output)
	}
}

func TestStandalonePhysicalPendingWithoutReceiptRefusesNoDebtCleanup(t *testing.T) {
	root := os.Getenv("HALRO_TERMINAL_ENOSPC_ROOT")
	if root == "" || os.Getenv("HALRO_TERMINAL_ENOSPC_FS") != "ext4" {
		t.Skip("requires dedicated ext4")
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC || uint64(fs.Blocks)*uint64(fs.Bsize) > 512<<20 {
		t.Fatal("unsafe filesystem", fs, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	directory := t.TempDir()
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
	cfg = activateClosedRuntimeMemoryStateFixture(t, ctx, cfg, instance, false)
	fixture := physicalAcceptedFixture{Config: cfg, Credentials: credentials, Filler: filepath.Join(directory, "foreign-filler"), RequestFile: filepath.Join(directory, "request-id")}
	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "fixture.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStandalonePhysicalAcceptedSIGKILLHelper$", "-test.count=1", "-test.v")
	child.Env = append(os.Environ(), "HALRO_PHYSICAL_ACCEPTED_CHILD="+path, "HALRO_PHYSICAL_ACCEPTED_MODE=crash")
	output, err := child.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("accepted SIGKILL failed: %v %s", err, output)
	}
	t.Logf("accepted original crash: %s", output)
	for bank := 0; bank < 2; bank++ {
		control := filepath.Join(cfg.Storage.DataDir, "memory-state", "ledger", fmt.Sprintf("terminal.physical.%d", bank))
		f, err := os.OpenFile(control, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := f.WriteAt(make([]byte, 4096), 0)
		if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
			t.Fatal(err)
		}
	}
	nativeBefore, err := os.Stat(cfg.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(cfg.Storage.DataDir, "memory-state", "ledger", "active.root")
	rootBefore, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	sealBefore, err := os.ReadFile(filepath.Join(cfg.Storage.DataDir, "active.seal"))
	if err != nil {
		t.Fatal(err)
	}
	var pools [2]os.FileInfo
	for i := range pools {
		pools[i], err = os.Stat(filepath.Join(cfg.Storage.DataDir, "memory-state", "ledger", fmt.Sprintf("terminal-root-%02d", i)))
		if err != nil {
			t.Fatal(err)
		}
	}
	reject := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStandalonePhysicalAcceptedSIGKILLHelper$", "-test.count=1", "-test.v")
	reject.Env = append(os.Environ(), "HALRO_PHYSICAL_ACCEPTED_CHILD="+path, "HALRO_PHYSICAL_ACCEPTED_MODE=reject")
	output, err = reject.CombinedOutput()
	t.Logf("independent no-authority refusal: %s", output)
	if err != nil {
		t.Fatal(err, string(output))
	}
	nativeAfter, err := os.Stat(cfg.LedgerPath())
	if err != nil || !os.SameFile(nativeBefore, nativeAfter) || nativeBefore.Size() != nativeAfter.Size() {
		t.Fatal("refusal modified native", err)
	}
	rootAfter, err := os.ReadFile(rootPath)
	if err != nil || !bytes.Equal(rootBefore, rootAfter) {
		t.Fatal("refusal changed physical COW root", err)
	}
	sealAfter, err := os.ReadFile(filepath.Join(cfg.Storage.DataDir, "active.seal"))
	if err != nil || !bytes.Equal(sealBefore, sealAfter) {
		t.Fatal("refusal changed common selection", err)
	}
	for i, expected := range pools {
		actual, err := os.Stat(filepath.Join(cfg.Storage.DataDir, "memory-state", "ledger", fmt.Sprintf("terminal-root-%02d", i)))
		if err != nil || !os.SameFile(expected, actual) || actual.Size() != 0 {
			t.Fatal("unqualified pending pool was deleted/replaced", err)
		}
	}
}
