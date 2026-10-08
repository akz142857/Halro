package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

func keyMaintenanceFixture(t *testing.T) (config.Config, context.Context, *resourcebudget.Manager) {
	t.Helper()
	cfg := commandBudgetFixture(t)
	cfg.ResourceBudget.MetadataStoreDiskBytes = 1 << 20
	addMetadataMappingFixtureRAM(t, cfg.ResourceBudget)
	cfg.ResourceBudget.RecoveryClaims = 16
	cfg.ResourceBudget.MaxClaims += 10
	cfg.ResourceBudget.ControlRAMBytes += 10 * resourcebudget.ClaimRAMBytes
	cfg.ResourceBudget.RecoveryReserveRAMBytes += 8 * resourcebudget.ClaimRAMBytes
	cfg.ResourceBudget.TotalRAMBytes += 20 * resourcebudget.ClaimRAMBytes
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, p, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, ctx, p.logicalBudget()
}
func TestKeyMaintenanceOriginalWorkspaceCoversActualOwnedFiles(t *testing.T) {
	cfg, root, manager := keyMaintenanceFixture(t)
	ctx, finish, err := keyMaintenanceContext(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := manager.Snapshot(); snapshot.Classes[resourcebudget.Recovery].RAM != keyMaintenanceRAMBytes {
		t.Fatal(snapshot)
	}
	nested, borrowed, err := keyMaintenanceContext(context.WithoutCancel(ctx), cfg)
	if err != nil || keyScope(nested) != keyScope(ctx) {
		t.Fatal("nested job changed root", err)
	}
	if err := borrowed(); err != nil {
		t.Fatal(err)
	}
	store, err := openCommandMetadataStore(ctx, cfg, cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	stage, err := newMetadataStagePath(cfg.Storage.DataDir, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("choosing name performed IO", err)
	}
	if _, err := snapshotCommandMetadata(ctx, cfg, store, stage); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	copied, err := openCommandMetadataPublish(ctx, cfg, stage)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := newMetadataStagePath(cfg.Storage.DataDir, "test-compact")
	if err != nil {
		t.Fatal(err)
	}
	if err := compactCommandMetadata(ctx, cfg, copied, compact); err != nil {
		t.Fatal(err)
	}
	if err := copied.Close(); err != nil {
		t.Fatal(err)
	}
	if err := publishKeyMetadata(ctx, compact, cfg.MetadataPath()); err != nil {
		t.Fatal(err)
	}
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned scratch not retired", err)
	}
	if _, err := os.Stat(cfg.MetadataPath()); err != nil {
		t.Fatal("published image removed", err)
	}
	snapshot := manager.Snapshot()
	if snapshot.Classes[resourcebudget.Recovery].RAM != 3*commandMetadataFileOwnerRAMBytes || snapshot.Classes[resourcebudget.Recovery].Disk != 3*cfg.ResourceBudget.MetadataStoreDiskBytes {
		t.Fatal("whole workspace/actual mappings were refunded before close or persistent file fees lost", snapshot)
	}
	store, err = boltstore.OpenReadOnly(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestKeyMaintenanceForeignStageAndTargetPreserved(t *testing.T) {
	for _, mode := range []string{"stage", "target"} {
		t.Run(mode, func(t *testing.T) {
			cfg, root, manager := keyMaintenanceFixture(t)
			ctx, finish, err := keyMaintenanceContext(root, cfg)
			if err != nil {
				t.Fatal(err)
			}
			store, err := openCommandMetadataStore(ctx, cfg, cfg.MetadataPath())
			if err != nil {
				t.Fatal(err)
			}
			stage, err := newMetadataStagePath(cfg.Storage.DataDir, "replace")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := snapshotCommandMetadata(ctx, cfg, store, stage); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			replacementPath := stage
			if mode == "target" {
				replacementPath = cfg.MetadataPath()
			}
			renamed := replacementPath + ".escaped"
			if err := os.Rename(replacementPath, renamed); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(replacementPath, []byte("foreign replacement"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := publishKeyMetadata(ctx, stage, cfg.MetadataPath()); !errors.Is(err, ownedfile.ErrReplaced) {
				t.Fatal("late replacement accepted", err)
			}
			finishErr := finish()
			if mode == "stage" {
				if !errors.Is(finishErr, errKeyMaintenanceRetirement) {
					t.Fatal("uncertain inode retirement succeeded", finishErr)
				}
				if manager.Snapshot().Classes[resourcebudget.Recovery].RAM < keyMaintenanceRAMBytes {
					t.Fatal("unknown retirement refunded workspace")
				}
			} else if finishErr != nil {
				t.Fatal(finishErr)
			}
			actual, err := os.ReadFile(replacementPath)
			if err != nil || string(actual) != "foreign replacement" {
				t.Fatal("foreign file removed", err)
			}
			if _, err := os.Stat(renamed); err != nil {
				t.Fatal("renamed original removed", err)
			}
		})
	}
}

type keyBlockedCloser struct {
	started, release chan struct{}
	once             sync.Once
	err              error
}

func (c *keyBlockedCloser) Close() error {
	c.once.Do(func() { close(c.started) })
	<-c.release
	return c.err
}
func TestKeyMaintenanceCancellationJoinsActualCloseAndKeepsUnknownFee(t *testing.T) {
	cfg, root, manager := keyMaintenanceFixture(t)
	parent, cancel := context.WithCancel(root)
	ctx, finish, err := keyMaintenanceContext(parent, cfg)
	if err != nil {
		t.Fatal(err)
	}
	blocker := &keyBlockedCloser{started: make(chan struct{}), release: make(chan struct{}), err: errors.New("uncertain physical close")}
	if err := keyScope(ctx).trackCloser(blocker); err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- finish() }()
	<-blocker.started
	select {
	case err := <-done:
		t.Fatal("close returned before actual writer exit", err)
	default:
	}
	if manager.Snapshot().Classes[resourcebudget.Recovery].RAM != keyMaintenanceRAMBytes {
		t.Fatal("cancellation refunded live workspace")
	}
	close(blocker.release)
	if err := <-done; !errors.Is(err, errKeyMaintenanceRetirement) {
		t.Fatal(err)
	}
	if manager.Snapshot().Classes[resourcebudget.Recovery].RAM != keyMaintenanceRAMBytes {
		t.Fatal("uncertain close refunded workspace")
	}
}
func TestKeyMaintenanceRefusesBeforeSourceIOAndForeignRoot(t *testing.T) {
	cfg := commandBudgetFixture(t)
	cfg.ResourceBudget.RecoveryRAMBytes = keyMaintenanceRAMBytes - 1
	cfg.Storage.DataDir = filepath.Join(t.TempDir(), "absent")
	if _, _, err := keyMaintenanceContext(context.Background(), cfg); err == nil {
		t.Fatal("undersized job accepted")
	}
	if _, err := os.Stat(cfg.Storage.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refusal entered source IO", err)
	}
	cfg, root, _ := keyMaintenanceFixture(t)
	ctx, finish, err := keyMaintenanceContext(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	other, _, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	bad := context.WithValue(other, keyMaintenanceContextKey{}, keyScope(ctx))
	if _, _, err := keyMaintenanceContext(bad, cfg); !errors.Is(err, resourcebudget.ErrInvalid) {
		t.Fatal("foreign original manager accepted", err)
	}
}

func TestKeyMaintenanceMasterKeyCloseUncertaintyRetainsWholeFee(t *testing.T) {
	cfg, root, manager := keyMaintenanceFixture(t)
	ctx, finish, err := keyMaintenanceContext(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	err = finishKeyMaintenance(ctx, finish, vault.ErrMasterKeyRetirement)
	if !errors.Is(err, vault.ErrMasterKeyRetirement) || manager.Snapshot().Classes[resourcebudget.Recovery].RAM != keyMaintenanceRAMBytes {
		t.Fatal("actual key Close uncertainty refunded whole job", err, manager.Snapshot())
	}
}
