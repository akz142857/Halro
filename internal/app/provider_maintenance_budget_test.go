package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/diskset"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func providerMaintenanceBudgetRuntime(t *testing.T) (*Runtime, BootstrapResult, *resourcebudget.Manager) {
	t.Helper()
	running, bootstrap, _ := activationTestRuntime(t)
	if err := running.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := running.config
	b := runtimeRAMBudgetFixture()
	b.TotalRAMBytes = 512 << 20
	b.IORAMBytes = 128 << 20
	b.TotalDiskBytes, b.IODiskBytes, b.ProviderObjectReferenceDiskBytes = 128<<20, 64<<20, 4<<20
	cfg.ResourceBudget = &b
	manager, err := resourcebudget.New(b.Limits(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	store, err := openMetadataForTest(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	r := &Runtime{config: cfg, store: store, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r.resourcePressure = &resourcePressureRuntime{budget: manager}
	return r, bootstrap, manager
}

func TestProviderSweepDiskReferencesPreserveNamedFreshAndUnknownObjects(t *testing.T) {
	r, bootstrap, manager := providerMaintenanceBudgetRuntime(t)
	directory := filepath.Join(r.config.Storage.DataDir, "provider-objects")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := 0; i < 131; i++ {
		resource := domain.ProviderResource{ID: fmt.Sprintf("provider-resource-%04d", i), Kind: domain.ResourceFile, ProjectID: bootstrap.ProjectID, ProviderID: bootstrap.ProviderID, DeploymentID: bootstrap.DeploymentID, ProfileID: domain.ProfileOpenAIMediaResources, PublicModel: "chat", ObjectPath: "named-output", InputObjectPath: "named-input", CreationStatus: "completed", Status: "uploaded", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
		if _, err := r.store.PutProviderResource(context.Background(), resource, 0); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name string, aged bool) {
		t.Helper()
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, append([]byte("HVLT"), 1, 0), 0600); err != nil {
			t.Fatal(err)
		}
		if aged {
			old := now.Add(-orphanSettlingPeriod - time.Hour)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := 0; i < 97; i++ {
		write(fmt.Sprintf("orphan-%03d", i), true)
	}
	for _, name := range []string{"named-output", "named-input", ".aged-partial"} {
		write(name, true)
	}
	write("fresh-output", false)
	write(".fresh-partial", false)
	if err := os.Mkdir(filepath.Join(directory, "unknown-directory"), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "foreign-target")
	if err := os.WriteFile(target, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(directory, "unknown-symlink")); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	t.Setenv("TMPDIR", parent)
	before, err := r.store.Info()
	if err != nil {
		t.Fatal(err)
	}
	var count uint64
	err = r.withProviderMaintenance(context.Background(), true, func(ctx context.Context, references *diskset.Set) error {
		s := manager.Snapshot()
		if s.Classes[resourcebudget.IO].RAM != providerMaintenanceRAMBytes || s.Classes[resourcebudget.IO].Disk != r.config.ResourceBudget.ProviderObjectReferenceDiskBytes || s.RecoveryClaims != 0 {
			t.Fatal("reference job escaped original IO pool", s)
		}
		var err error
		count, err = r.sweepProviderObjects(ctx, references)
		return err
	})
	if err != nil || count != 98 {
		t.Fatal("bounded complete sweep", count, err)
	}
	for _, name := range []string{"named-output", "named-input", "fresh-output", ".fresh-partial", "unknown-directory", "unknown-symlink"} {
		if _, err := os.Lstat(filepath.Join(directory, name)); err != nil {
			t.Fatal("protected object removed", name, err)
		}
	}
	after, err := r.store.Info()
	if err != nil || after != before {
		t.Fatal("deletion gate changed native Metadata", before, after, err)
	}
	if files, err := os.ReadDir(parent); err != nil || len(files) != 0 || manager.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("actual reference retirement retained files or fees", files, manager.Snapshot(), err)
	}
}

func TestProviderMaintenanceSharedRefusalCancellationAndUncertainRetirement(t *testing.T) {
	r, _, manager := providerMaintenanceBudgetRuntime(t)
	r.maintenanceOwners = newRuntimeMaintenanceOwners(r.config, r.resourcePressure)
	// Substitute an empty pool at the caller-facing pointer. Published work
	// must still contend with the original owner and retain its original limit.
	substitute, err := resourcebudget.New(r.config.ResourceBudget.Limits(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := substitute.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	r.resourcePressure = &resourcePressureRuntime{budget: substitute}
	parent := t.TempDir()
	t.Setenv("TMPDIR", parent)
	occupied, err := manager.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: 100 << 20})
	if err != nil {
		t.Fatal(err)
	}
	err = r.withProviderMaintenance(context.Background(), true, func(context.Context, *diskset.Set) error { t.Fatal("refused job reached source IO"); return nil })
	if !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal(err)
	}
	if substitute.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("maintenance admitted on substitute manager", substitute.Snapshot())
	}
	if files, err := os.ReadDir(parent); err != nil || len(files) != 0 {
		t.Fatal("refusal created scratch", files, err)
	}
	if err := occupied.Release(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	entered, resume, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- r.withProviderMaintenance(ctx, true, func(context.Context, *diskset.Set) error { close(entered); <-resume; return nil })
	}()
	<-entered
	cancel()
	if s := manager.Snapshot(); s.Classes[resourcebudget.IO].RAM != providerMaintenanceRAMBytes || s.NormalClaims != 1 {
		close(resume)
		<-done
		t.Fatal("cancel refunded retained physical job", s)
	}
	close(resume)
	if err := <-done; !errors.Is(err, context.Canceled) || manager.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal(err, manager.Snapshot())
	}
	var foreign string
	err = r.withProviderMaintenance(context.Background(), true, func(context.Context, *diskset.Set) error {
		paths, err := filepath.Glob(filepath.Join(parent, "halro-provider-references-*", "references"))
		if err != nil || len(paths) != 1 {
			t.Fatal(paths, err)
		}
		foreign = paths[0]
		if err := os.Rename(foreign, filepath.Join(parent, "owned-outside")); err != nil {
			return err
		}
		return os.WriteFile(foreign, []byte("foreign"), 0600)
	})
	s := manager.Snapshot()
	if err == nil || s.Classes[resourcebudget.IO].RAM != providerMaintenanceRAMBytes || s.Classes[resourcebudget.IO].Disk != r.config.ResourceBudget.ProviderObjectReferenceDiskBytes {
		t.Fatal("uncertain original inode refunded its job", s, err)
	}
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "foreign" {
		t.Fatal("foreign replacement removed", data, err)
	}
}
