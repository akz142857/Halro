//go:build linux

package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/masterkey"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/vault"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

// This is an actual ext4 functionality fixture. The pressure source supplied by
// the original fixture is functional evidence, and is not a capacity claim.
func TestSelectedPhysicalFileRotationActualRuntimeLatestFacts(t *testing.T) {
	root := os.Getenv("HALRO_EXT4_ROOT")
	if root == "" {
		t.Skip("requires explicit dedicated ext4 fixture")
	}
	var fs unix.Statfs_t
	if !filepath.IsAbs(root) {
		t.Fatal("fixture root must be absolute")
	}
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC {
		t.Fatal("requires actual ext4", fs, err)
	}
	t.Setenv("TMPDIR", root)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cfg, _, _, sample := selectedLegacyExportFixture(t, ctx, false)
	limits := physicalTerminalBudgetFixture()
	limits.PhysicalTerminalReserve = true
	limits.MemoryStateRequired = true
	// Thirty-two real Ledger/Usage future-root slots and the source builder caps
	// are retained; the filesystem fixture size is independent of logical fees.
	cfg.ResourceBudget = &limits
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := OpenWithOptions(ctx, cfg, logger, OpenOptions{offlineVerification: true, AllowGeneratedSetupToken: true, resourceSampleSource: sample})
	if err != nil {
		t.Fatal("original physical Runtime", err)
	}
	defer r.Close()
	if err = r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	oldKey, err := unlockMasterKey(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(oldKey)
	v, err := vault.New(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	c := domain.Credential{ID: "cred_physical_rotation_latest", Name: "latest", Type: domain.ProviderOpenAI, Audience: "https://api.openai.com", KeyVersion: 1, CreatedAt: time.Now().UTC()}
	secret := []byte("actual physical selected latest credential")
	c.Ciphertext, err = v.EncryptCredential(c.ID, string(c.Type), c.Audience, secret)
	v.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.store.PutCredential(ctx, c, 0, nil); err != nil {
		t.Fatal("latest physical Metadata fact", err)
	}
	nativeKey := bytes.Clone(r.selectedState.bootstrap.LedgerKey)
	defer clear(nativeKey)
	keyGeneration := r.selectedState.bootstrap.KeyGeneration
	if err = r.CheckpointMemoryState(ctx); err != nil {
		t.Fatal("original physical common", err)
	}
	if err = r.Close(); err != nil {
		t.Fatal("original physical Runtime close", err)
	}
	next := filepath.Join(t.TempDir(), "next.key")
	if err = vault.InitMasterKey(next); err != nil {
		t.Fatal(err)
	}
	rotated, err := rotateSelectedMasterKey(ctx, cfg, next, func(phase string) error { t.Log("rotation stage", phase); return nil }, sample)
	if err != nil {
		t.Fatal("actual physical File rotation", err)
	}
	if rotated.NewKeyVersion != 2 {
		t.Fatal(rotated)
	}
	fresh, err := OpenWithOptions(ctx, cfg, logger, OpenOptions{AllowGeneratedSetupToken: true, resourceSampleSource: sample})
	if err != nil {
		t.Fatal("fresh actual physical Runtime", err)
	}
	defer fresh.Close()
	fresh.stopBackgroundWork() // Actual ordinary Open; join ancillary workers before controlled maintenance.
	if err = fresh.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(nativeKey, fresh.selectedState.bootstrap.LedgerKey) || keyGeneration != fresh.selectedState.bootstrap.KeyGeneration {
		t.Fatal("rotation changed native authority")
	}
	actual, err := fresh.store.GetCredential(ctx, c.ID)
	if err != nil {
		t.Fatal("lost latest fact", err)
	}
	nextKey, err := vault.LoadMasterKey(next)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(nextKey)
	nv, err := vault.New(nextKey)
	if err != nil {
		t.Fatal(err)
	}
	defer nv.Close()
	plain, err := nv.DecryptCredential(actual.ID, string(actual.Type), actual.Audience, actual.Ciphertext)
	if err != nil || !bytes.Equal(plain, secret) || actual.KeyVersion != 2 {
		t.Fatal("new DEK latest fact", err)
	}
	clear(plain)
	ov, err := vault.New(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	defer ov.Close()
	if _, err = ov.DecryptCredential(actual.ID, string(actual.Type), actual.Audience, actual.Ciphertext); err == nil {
		t.Fatal("old DEK decrypts new current ciphertext")
	}
	if err = fresh.CheckpointMemoryState(ctx); err != nil {
		t.Fatal("fresh physical common", err)
	}
	if err = fresh.Close(); err != nil {
		t.Fatal("fresh physical Runtime close", err)
	}
}

func TestSelectedPhysicalKMSRotationActualRuntimeAndPendingResume(t *testing.T) {
	requireSelectedPhysicalRotationExt4(t)
	for _, phase := range []string{"", "selected_before_seal", "selected_after_seal", "selected_after_unlock"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			harness := newKMSAppHarness(t)
			ctx = context.WithValue(ctx, selectedRotationKMSFactoryKey{}, kmsWrapperFactory(harness.factory))
			cfg, sample := selectedRotationKMSFixture(t, ctx, harness)
			limits := physicalTerminalBudgetFixture()
			limits.PhysicalTerminalReserve = true
			limits.MemoryStateRequired = true
			cfg.ResourceBudget = &limits
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			r, err := OpenWithOptions(ctx, cfg, logger, OpenOptions{offlineVerification: true, AllowGeneratedSetupToken: true, resourceSampleSource: sample})
			if err != nil {
				t.Fatal("original physical KMS Runtime", err)
			}
			defer r.Close()
			p := domain.Project{ID: "prj_physical_kms_latest", Name: "actual selected latest KMS fact", Enabled: true}
			if _, err = r.store.PutProject(ctx, p, 0, nil); err != nil {
				t.Fatal(err)
			}
			nativeKey := bytes.Clone(r.selectedState.bootstrap.LedgerKey)
			defer clear(nativeKey)
			nativeGeneration := r.selectedState.bootstrap.KeyGeneration
			if err = r.CheckpointMemoryState(ctx); err != nil {
				t.Fatal(err)
			}
			if err = r.Close(); err != nil {
				t.Fatal(err)
			}
			physicalConfig := filepath.Join(t.TempDir(), "original-physical-kms.yaml")
			rawConfig, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(physicalConfig, rawConfig, 0600); err != nil {
				t.Fatal(err)
			}
			killSelectedPhysicalOriginalRuntime(t, ctx, cfg, physicalConfig)
			interrupted := errors.New("stop physical KMS rotation at actual persisted stage")
			o := kmsRotationOptions{factory: harness.factory, operationID: "physical-selected-kms", random: bytes.NewReader(bytes.Repeat([]byte{0x92}, 32)), hook: func(actual string) error {
				if phase != "" && actual == phase {
					return interrupted
				}
				return nil
			}}
			result, err := rotateSelectedKMSMasterKey(ctx, cfg, o, sample)
			if phase != "" {
				if !errors.Is(err, interrupted) {
					t.Fatal("actual stage not reached", err)
				}
				if escaped, gateErr := OpenWithOptions(ctx, cfg, logger, OpenOptions{offlineVerification: true, AllowGeneratedSetupToken: true, resourceSampleSource: sample}); escaped != nil || !errors.Is(gateErr, ErrSelectedKeyRotationPending) {
					if escaped != nil {
						escaped.Close()
					}
					t.Fatal("pending physical KMS allowed Runtime", gateErr)
				}
				o.hook = nil
				result, err = rotateSelectedKMSMasterKey(ctx, cfg, o, sample)
				if !result.RecoveredPending {
					t.Fatal("same actual physical KMS transaction not resumed", result, err)
				}
			}
			if err != nil || result.NewKeyVersion != 2 {
				t.Fatal(result, err)
			}
			fresh, err := OpenWithOptions(ctx, cfg, logger, OpenOptions{AllowGeneratedSetupToken: true, resourceSampleSource: sample})
			if err != nil {
				t.Fatal("fresh physical KMS Runtime", err)
			}
			defer fresh.Close()
			fresh.stopBackgroundWork() // Actual ordinary Open; join ancillary workers before controlled maintenance.
			if err = fresh.selectedState.generation.WaitStartupAudit(ctx); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(nativeKey, fresh.selectedState.bootstrap.LedgerKey) || nativeGeneration != fresh.selectedState.bootstrap.KeyGeneration {
				t.Fatal("KMS switched native HMAC authority")
			}
			if got, err := fresh.store.GetProject(ctx, p.ID); err != nil || got.Name != p.Name {
				t.Fatal("lost actual KMS latest fact", got, err)
			}
			d, err := fresh.store.KeySlotDescriptor(ctx)
			if err != nil || !d.ProductionReady() || d.ActiveGeneration != 2 {
				t.Fatal("actual selected keyslot descriptor", d, err)
			}
			for _, purpose := range []masterkey.KeySlotPurpose{masterkey.KeySlotPrimary, masterkey.KeySlotRecovery} {
				key, err := unlockKMSMasterKey(ctx, cfg, fresh.store, purpose, harness.factory)
				if err != nil || !bytes.Equal(key, bytes.Repeat([]byte{0x92}, 32)) {
					clear(key)
					t.Fatal("actual configured slot failed new DEK unlock", purpose, err)
				}
				clear(key)
			}
			if err = fresh.CheckpointMemoryState(ctx); err != nil {
				t.Fatal("fresh physical KMS common", err)
			}
			if err = fresh.Close(); err != nil {
				t.Fatal("fresh physical KMS Close", err)
			}
		})
	}
}

func requireSelectedPhysicalRotationExt4(t *testing.T) {
	t.Helper()
	root := os.Getenv("HALRO_EXT4_ROOT")
	if root == "" {
		t.Skip("requires explicit dedicated ext4 fixture")
	}
	var fs unix.Statfs_t
	if !filepath.IsAbs(root) {
		t.Fatal("fixture root must be absolute")
	}
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC {
		t.Fatal("requires actual ext4", fs, err)
	}
	t.Setenv("TMPDIR", root)
}

func TestSelectedPhysicalFileRotationActualSIGKILLAndDefaultReopen(t *testing.T) {
	requireSelectedPhysicalRotationExt4(t)
	for _, phase := range []string{"selected_before_seal", "selected_after_seal", "selected_after_bootstrap", "selected_after_key", "selected_before_old_root_retirement"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			cfg, _, cfgPath, sample := selectedLegacyExportFixture(t, ctx, false)
			limits := physicalTerminalBudgetFixture()
			limits.PhysicalTerminalReserve = true
			limits.MemoryStateRequired = true
			cfg.ResourceBudget = &limits
			raw, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(cfgPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			r, err := OpenWithOptions(ctx, cfg, logger, OpenOptions{offlineVerification: true, AllowGeneratedSetupToken: true, resourceSampleSource: sample})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			p := domain.Project{ID: "prj_physical_kill_latest", Name: "latest physical SIGKILL fact", Enabled: true}
			if _, err = r.store.PutProject(ctx, p, 0, nil); err != nil {
				t.Fatal(err)
			}
			nativeKey := bytes.Clone(r.selectedState.bootstrap.LedgerKey)
			defer clear(nativeKey)
			if err = r.CheckpointMemoryState(ctx); err != nil {
				t.Fatal(err)
			}
			if err = r.Close(); err != nil {
				t.Fatal(err)
			}
			next := filepath.Join(t.TempDir(), "next.key")
			if err = vault.InitMasterKey(next); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "reached")
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSelectedKeyRotationSIGKILLChild$", "-test.count=1")
			command.Env = append(os.Environ(), "HALRO_SELECTED_ROTATION_CHILD_CONFIG="+cfgPath, "HALRO_SELECTED_ROTATION_CHILD_PHASE="+phase, "HALRO_SELECTED_ROTATION_CHILD_MARKER="+marker, "HALRO_SELECTED_ROTATION_CHILD_KEY="+next)
			var output bytes.Buffer
			command.Stdout = &output
			command.Stderr = &output
			if err = command.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				if _, err = os.Lstat(marker); err == nil {
					break
				}
				select {
				case err := <-done:
					t.Fatal("actual child exited before kill boundary", phase, err, output.String())
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-ticker.C:
				}
			}
			if err = command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err = <-done; err == nil {
				t.Fatal("child not killed")
			}
			if raw, err = os.ReadFile(marker); err != nil || string(raw) != phase {
				t.Fatal("wrong actual kill boundary", err)
			}
			if escaped, gateErr := OpenWithOptions(ctx, cfg, logger, OpenOptions{offlineVerification: true, AllowGeneratedSetupToken: true, resourceSampleSource: sample}); escaped != nil || !errors.Is(gateErr, ErrSelectedKeyRotationPending) {
				if escaped != nil {
					escaped.Close()
				}
				t.Fatal("physical pending allowed default Runtime", gateErr)
			}
			result, err := rotateSelectedMasterKey(ctx, cfg, next, nil, sample)
			if err != nil || !result.RecoveredPending {
				t.Fatal("actual killed physical transaction did not resume", phase, result, err)
			}
			fresh, err := OpenWithOptions(ctx, cfg, logger, OpenOptions{AllowGeneratedSetupToken: true, resourceSampleSource: sample})
			if err != nil {
				t.Fatal("fresh default physical Runtime", err)
			}
			defer fresh.Close()
			fresh.stopBackgroundWork() // Actual ordinary Open; join ancillary workers before controlled maintenance.
			if err = fresh.selectedState.generation.WaitStartupAudit(ctx); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(nativeKey, fresh.selectedState.bootstrap.LedgerKey) || fresh.selectedState.bootstrap.VaultVersion != 2 {
				t.Fatal("wrong native or Vault authority")
			}
			if got, err := fresh.store.GetProject(ctx, p.ID); err != nil || got.Name != p.Name {
				t.Fatal("actual SIGKILL lost latest fact", err)
			}
			if err = fresh.CheckpointMemoryState(ctx); err != nil {
				t.Fatal(err)
			}
			if err = fresh.Close(); err != nil {
				t.Fatal("fresh actual physical Close", err)
			}
		})
	}
}

func TestSelectedPhysicalRotationOriginalRuntimeSIGKILLChild(t *testing.T) {
	cfgPath := os.Getenv("HALRO_PHYSICAL_ROTATION_ORIGINAL_CONFIG")
	if cfgPath == "" {
		t.Skip("subprocess only")
	}
	cfg, err := config.Load(cfgPath, config.LoadOptions{SkipListenerValidation: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if cfg.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots {
		harness := newKMSAppHarness(t)
		ctx = context.WithValue(ctx, selectedRotationKMSFactoryKey{}, kmsWrapperFactory(harness.factory))
	}
	sample := pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })
	r, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{offlineVerification: true, AllowGeneratedSetupToken: true, resourceSampleSource: sample})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err = r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	p := domain.Project{ID: "prj_physical_original_killed", Name: "fact from original killed physical Runtime", Enabled: true}
	if _, err = r.store.PutProject(ctx, p, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err = r.CheckpointMemoryState(ctx); err != nil {
		t.Fatal(err)
	}
	marker := os.Getenv("HALRO_PHYSICAL_ROTATION_ORIGINAL_MARKER")
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("original-physical-checkpoint-complete"); err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(f.Sync(), f.Close()); err != nil {
		t.Fatal(err)
	}
	// Keep the actual Runtime, physical receipts, FD owners and monitor alive.
	// Only the parent SIGKILL ends this child; no defer is reached.
	for {
		time.Sleep(time.Minute)
	}
}

func TestSelectedPhysicalFileRotationAfterOriginalRuntimeSIGKILL(t *testing.T) {
	requireSelectedPhysicalRotationExt4(t)
	for _, phase := range []string{"", "selected_before_seal", "seal-renamed", "seal-directory-synced", "selected_after_seal", "selected_after_key"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			cfg, _, cfgPath, sample := selectedLegacyExportFixture(t, ctx, false)
			limits := physicalTerminalBudgetFixture()
			limits.PhysicalTerminalReserve = true
			limits.MemoryStateRequired = true
			cfg.ResourceBudget = &limits
			raw, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(cfgPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "original-live")
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSelectedPhysicalRotationOriginalRuntimeSIGKILLChild$", "-test.count=1")
			command.Env = append(os.Environ(), "HALRO_PHYSICAL_ROTATION_ORIGINAL_CONFIG="+cfgPath, "HALRO_PHYSICAL_ROTATION_ORIGINAL_MARKER="+marker)
			var output bytes.Buffer
			command.Stdout = &output
			command.Stderr = &output
			if err = command.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				if _, err = os.Lstat(marker); err == nil {
					break
				}
				select {
				case err := <-done:
					t.Fatal("original physical Runtime escaped", err, output.String())
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-ticker.C:
				}
			}
			// These controls were actually owned by the killed Runtime, rather than
			// recreated later merely from cfg.PhysicalTerminalReserve.
			for _, dir := range []string{"ledger", "usage", "directory", "metadata"} {
				root := filepath.Join(memoryStateDirectory(cfg), dir)
				if dir == "metadata" {
					root, _ = memoryStateMetadataPaths(cfg, 0)
				}
				entries, err := os.ReadDir(root)
				if err != nil {
					t.Fatal(err)
				}
				present := false
				for _, entry := range entries {
					present = present || strings.HasPrefix(entry.Name(), "terminal")
				}
				if !present {
					t.Fatal("actual original physical control absent", dir)
				}
			}
			if err = command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err = <-done; err == nil {
				t.Fatal("original not killed")
			}
			next := filepath.Join(t.TempDir(), "next.key")
			if err = vault.InitMasterKey(next); err != nil {
				t.Fatal(err)
			}
			if phase != "" {
				killSelectedPhysicalRotationChild(t, ctx, cfgPath, next, phase)
			}
			result, err := rotateSelectedMasterKey(ctx, cfg, next, nil, sample)
			if err != nil || result.NewKeyVersion != 2 {
				t.Fatal("rotation from actual unjoined original physical receipts", result, err)
			}
			fresh, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{AllowGeneratedSetupToken: true, resourceSampleSource: sample})
			if err != nil {
				t.Fatal(err)
			}
			defer fresh.Close()
			fresh.stopBackgroundWork()
			if err = fresh.selectedState.generation.WaitStartupAudit(ctx); err != nil {
				t.Fatal(err)
			}
			if got, err := fresh.store.GetProject(ctx, "prj_physical_original_killed"); err != nil || got.Name != "fact from original killed physical Runtime" {
				t.Fatal("lost killed original fact", got, err)
			}
			if err = fresh.CheckpointMemoryState(ctx); err != nil {
				t.Fatal(err)
			}
			if err = fresh.Close(); err != nil {
				t.Fatal("fresh physical owner Close", err)
			}
		})
	}
}

func killSelectedPhysicalRotationChild(t *testing.T, ctx context.Context, cfgPath, next, phase string) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "rotation-target-reached")
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSelectedKeyRotationSIGKILLChild$", "-test.count=1")
	physical := ""
	if phase == "seal-renamed" || phase == "seal-directory-synced" {
		physical = phase
	}
	command.Env = append(os.Environ(), "HALRO_SELECTED_ROTATION_CHILD_CONFIG="+cfgPath, "HALRO_SELECTED_ROTATION_CHILD_PHASE="+phase, "HALRO_SELECTED_ROTATION_CHILD_PHYSICAL_PHASE="+physical, "HALRO_SELECTED_ROTATION_CHILD_MARKER="+marker, "HALRO_SELECTED_ROTATION_CHILD_KEY="+next)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Lstat(marker); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatal("rotation exited before actual target boundary", phase, err, output.String())
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("rotation child was not killed")
	}
	if raw, err := os.ReadFile(marker); err != nil || string(raw) != phase {
		t.Fatal("wrong physical rotation boundary", err)
	}
}

func killSelectedPhysicalOriginalRuntime(t *testing.T, ctx context.Context, cfg config.Config, cfgPath string) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "original-physical-source")
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSelectedPhysicalRotationOriginalRuntimeSIGKILLChild$", "-test.count=1")
	command.Env = append(os.Environ(), "HALRO_PHYSICAL_ROTATION_ORIGINAL_CONFIG="+cfgPath, "HALRO_PHYSICAL_ROTATION_ORIGINAL_MARKER="+marker)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Lstat(marker); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatal("original configured physical Runtime escaped", err, output.String())
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	for _, dir := range []string{"ledger", "usage", "directory", "metadata"} {
		root := filepath.Join(memoryStateDirectory(cfg), dir)
		if dir == "metadata" {
			root, _ = memoryStateMetadataPaths(cfg, 0)
		}
		for _, bank := range []string{"terminal.physical.0", "terminal.physical.1"} {
			info, err := os.Lstat(filepath.Join(root, bank))
			if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
				t.Fatal("original actual physical bank absent", dir, bank, err)
			}
		}
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("original actual physical Runtime was not killed")
	}
}
