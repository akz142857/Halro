package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/diskinventory"
	"github.com/akz142857/Halro/internal/resourcebudget"
	storelock "github.com/akz142857/Halro/internal/store/lock"
	"gopkg.in/yaml.v3"
)

func runtimeInventoryBudgetFixture() config.ResourceBudget {
	b := runtimeRAMBudgetFixture()
	b.PersistentInventoryDiskBytes = 64 << 20
	b.TotalDiskBytes, b.BackgroundDiskBytes = 4<<30, 1<<30
	b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes = 1<<30, 1<<30
	b.RecoveryClaims++
	b.MaxClaims += 2
	b.ControlRAMBytes += 2 * resourcebudget.ClaimRAMBytes
	// Reopening a real archive admits the complete comparison source job as
	// well as the inventory. These are actual shared startup envelopes.
	b.RecoveryRAMBytes += (512 << 20) + diskinventory.WorkspaceRAMBytes
	b.RecoveryReserveRAMBytes += (512 << 20) + diskinventory.WorkspaceRAMBytes + resourcebudget.ClaimRAMBytes
	b.TotalRAMBytes += (512 << 20) + diskinventory.WorkspaceRAMBytes + 2*resourcebudget.ClaimRAMBytes
	return b
}

func TestRuntimePersistentInventoryDefaultOpenCloseAndFreshManagerRebuild(t *testing.T) {
	r, _, reopen := openBootstrappedRuntimeShaped(t, "gpt-test", func(cfg *config.Config) {
		pressure, b := runtimePressureFixture(), runtimeInventoryBudgetFixture()
		cfg.ResourcePressure, cfg.ResourceBudget = &pressure, &b
	})
	if r.persistentInventory == nil {
		t.Fatal("default Runtime did not retain the actual inventory baseline")
	}
	first := r.persistentInventory.Snapshot()
	manager := r.resourcePressure.logicalBudget()
	if first.Files == 0 || first.ChargeBytes == 0 || manager.Snapshot().Classes[resourcebudget.Background].Disk != first.ChargeBytes {
		t.Fatalf("default startup lost actual baseline %+v %+v", first, manager.Snapshot())
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if got := manager.Snapshot().Used; got != (resourcebudget.Cost{RAM: resourcebudget.ClaimRAMBytes, Disk: first.ChargeBytes}) {
		t.Fatalf("Runtime Close refunded persisted tree %+v", got)
	}
	// A failed private replacement appears AFTER the first Manager's scan.
	// The next Manager must observe it rather than inherit old saved counts.
	private := filepath.Join(r.config.Storage.DataDir, "failed-replacement.pending")
	if err := os.WriteFile(private, []byte(strings.Repeat("x", 1<<20)), 0600); err != nil {
		t.Fatal(err)
	}
	round := reopen()
	t.Cleanup(func() { _ = round.Close() })
	if round.resourcePressure.logicalBudget() == manager || round.persistentInventory == nil {
		t.Fatal("reopen reused the old Manager/inventory owner")
	}
	second := round.persistentInventory.Snapshot()
	if second.ChargeBytes <= first.ChargeBytes || second.LogicalBytes <= first.LogicalBytes {
		t.Fatalf("new Manager ignored actual later replacement %+v %+v", first, second)
	}
	if err := round.Close(); err != nil {
		t.Fatal(err)
	}
	if got := round.resourcePressure.logicalBudget().Snapshot().Used; got != (resourcebudget.Cost{RAM: resourcebudget.ClaimRAMBytes, Disk: second.ChargeBytes}) {
		t.Fatalf("second Runtime lost persistent fee %+v", got)
	}
}

func TestRuntimePersistentInventoryCapacityRejectsBeforeNativeTailRepair(t *testing.T) {
	seed, _, _ := openBootstrappedRuntime(t)
	cfg := seed.config
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	path := cfg.AuditPath()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0xff, 0x01}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pressure, b := runtimePressureFixture(), runtimeInventoryBudgetFixture()
	b.PersistentInventoryDiskBytes = 1
	cfg.ResourcePressure, cfg.ResourceBudget = &pressure, &b
	if err := cfg.Validate(config.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	r, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if r != nil || !errors.Is(err, diskinventory.ErrLimit) {
		t.Fatalf("native opened before inventory capacity gate %v %v", r, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatalf("inventory refusal repaired native tail %v", err)
	}
	// The actual DataDir lock must have exited on failed startup.
	g, err := storelock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimePersistentInventoryOutsideNativePathRejectedBeforeScanning(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	pressure, b := runtimePressureFixture(), runtimeInventoryBudgetFixture()
	cfg.ResourcePressure, cfg.ResourceBudget = &pressure, &b
	foreign := filepath.Join(filepath.Dir(cfg.Storage.DataDir), "foreign-native.db")
	if err := os.WriteFile(foreign, []byte("foreign native authority"), 0600); err != nil {
		t.Fatal(err)
	}
	// Deliberately bypass Config.Validate; the Runtime owner must enforce its
	// own coverage boundary even when passed a manufactured Config value.
	cfg.Storage.MetadataFile = "../foreign-native.db"
	g, err := storelock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	manager, err := resourcebudget.New(b.Limits(pressure.MaxAge.Value()))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := preparePersistentInventory(context.Background(), cfg, g, &resourcePressureRuntime{budget: manager})
	if owner != nil || err == nil || !strings.Contains(err.Error(), "outside configured persistent inventory tree") {
		t.Fatalf("outside native owner accepted %v %v", owner, err)
	}
	if manager.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("coverage refusal created a partial shared claim", manager.Snapshot())
	}
	bytes, err := os.ReadFile(foreign)
	if err != nil || string(bytes) != "foreign native authority" {
		t.Fatalf("outside native file touched %q %v", bytes, err)
	}
}

func TestRuntimePersistentInventoryDisabledPreservesExistingStartupPath(t *testing.T) {
	cfg := testConfig(t)
	b := runtimeRAMBudgetFixture()
	cfg.ResourceBudget = &b
	owner, err := preparePersistentInventory(context.Background(), cfg, nil, nil)
	if owner != nil || err != nil {
		t.Fatal("zero inventory opted into namespace inspection", owner, err)
	}
}

func TestRuntimePersistentInventoryLaterStartupFailureRetainsBaselineOwner(t *testing.T) {
	seed, _, _ := openBootstrappedRuntime(t)
	cfg := seed.config
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	pressure, b := runtimePressureFixture(), runtimeInventoryBudgetFixture()
	cfg.ResourcePressure, cfg.ResourceBudget = &pressure, &b
	cfg.Storage.MasterKey.File = filepath.Join(t.TempDir(), "missing-master.key")
	r, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if r != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("later startup error lost cause %v %v", r, err)
	}
	var failure *persistentInventoryFailure
	if !errors.As(err, &failure) || failure.owner == nil || failure.owner.Snapshot().ChargeBytes == 0 {
		t.Fatalf("later native startup failure dropped already-published persistent owner %v", err)
	}
	if err := failure.owner.Close(); err != nil {
		t.Fatal(err)
	}
	if failure.owner.Snapshot().ChargeBytes == 0 {
		t.Fatal("failed Runtime Close erased owned persistent baseline")
	}
	g, err := storelock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
}

type processInventoryResult struct {
	Observed   diskinventory.Snapshot
	AfterClose resourcebudget.Cost
}

func TestRuntimePersistentInventoryRebuildsAfterActualProcessExit(t *testing.T) {
	const configEnv = "HALRO_TEST_INVENTORY_CHILD_CONFIG"
	if path := os.Getenv(configEnv); path != "" {
		file, err := os.Open(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		cfg, err := config.Decode(file)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			fmt.Fprintln(os.Stderr, errors.Join(err, closeErr))
			os.Exit(2)
		}
		r, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if r.persistentInventory == nil {
			fmt.Fprintln(os.Stderr, "missing child inventory")
			os.Exit(2)
		}
		observed := r.persistentInventory.Snapshot()
		if err := r.Close(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		result := processInventoryResult{Observed: observed, AfterClose: r.resourcePressure.logicalBudget().Snapshot().Used}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	r, _, _ := openBootstrappedRuntimeShaped(t, "gpt-test", func(cfg *config.Config) {
		pressure, b := runtimePressureFixture(), runtimeInventoryBudgetFixture()
		cfg.ResourcePressure, cfg.ResourceBudget = &pressure, &b
	})
	first := r.persistentInventory.Snapshot()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	late := filepath.Join(r.config.Storage.DataDir, "predecessor-process.pending")
	if err := os.WriteFile(late, []byte(strings.Repeat("p", 1<<20)), 0600); err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(r.config)
	if err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(t.TempDir(), "child-config.yaml")
	if err := os.WriteFile(configFile, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRuntimePersistentInventoryRebuildsAfterActualProcessExit$")
	cmd.Env = append(os.Environ(), configEnv+"="+configFile)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("actual child Runtime: %v %s", err, output)
	}
	var result processInventoryResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("child observation %v %s", err, output)
	}
	if result.Observed.ChargeBytes <= first.ChargeBytes || result.Observed.LogicalBytes <= first.LogicalBytes {
		t.Fatalf("actual new process trusted preceding inventory %+v %+v", first, result)
	}
	if result.AfterClose != (resourcebudget.Cost{RAM: resourcebudget.ClaimRAMBytes, Disk: result.Observed.ChargeBytes}) {
		t.Fatalf("child Close refunded existing tree %+v", result)
	}
}
