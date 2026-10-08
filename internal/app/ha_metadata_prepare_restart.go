package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/diskinventory"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

// HAMetadataPrepareOptions is an explicit offline operator decision. Restart
// grants no authority to the interrupted candidate or any file inside it.
type HAMetadataPrepareOptions struct{ RestartSource bool }

// PrepareHAMetadataBaselineWithOptions creates the reserved workspace only
// after authenticating the real member and complete old native ordering cut.
// Restart preserves an existing namespace in one of four fixed opaque slots;
// the candidate is rebuilt from the caller's independently verified archive.
func PrepareHAMetadataBaselineWithOptions(ctx context.Context, cfg config.Config, keys HAMetadataKeys, archive *backup.VerifiedArchive, workspace string, guard *lock.Lock, o metadatastate.BaselineOptions, options HAMetadataPrepareOptions) (*HAMetadataPreparation, error) {
	return prepareHAMetadataBaseline(ctx, cfg, keys, archive, workspace, guard, o, options, true, nil)
}

func haMetadataPreparePhase(phase func(string) error, name string) error {
	if phase != nil {
		return phase(name)
	}
	return nil
}

func (p *HAMetadataPreparation) prepareWorkspace(ctx context.Context, cfg config.Config, workspace string, guard *lock.Lock, manager *resourcebudget.Manager, options HAMetadataPrepareOptions, phase func(string) error) (*diskinventory.Baseline, error) {
	captured, err := os.Lstat(workspace)
	if !options.RestartSource && !errors.Is(err, os.ErrNotExist) {
		return nil, errors.Join(os.ErrExist, err)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	destination := ""
	if captured != nil {
		if _, _, err := migrationDirectoryIdentity(captured); err != nil {
			return nil, err
		}
		for i := 0; i < 4; i++ {
			path := fmt.Sprintf("%s.source-unqualified-%d", workspace, i)
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				destination = path
				break
			} else if err != nil {
				return nil, err
			}
		}
		if destination == "" {
			return nil, errors.New("HA Metadata interrupted source slots exhausted; retained namespaces require explicit archival")
		}
	}
	// Inventory describes physical ownership costs, never candidate trust. It
	// includes unknown files and every opaque slot before admitting fresh growth.
	inventory, err := diskinventory.Reconcile(ctx, diskinventory.Options{DataDir: cfg.Storage.DataDir, Guard: guard, Budget: manager, Ceiling: cfg.ResourceBudget.PersistentInventoryDiskBytes, Deadline: time.Minute})
	if err != nil {
		return inventory, err
	}
	if err := inventory.Publish(); err != nil {
		return inventory, err
	}
	if err := ctx.Err(); err != nil {
		return inventory, err
	}
	if captured != nil {
		named, err := os.Lstat(workspace)
		if err != nil || !os.SameFile(captured, named) || !guard.CoversDataDirectory(cfg.Storage.DataDir) {
			return inventory, errors.Join(ownedfile.ErrReplaced, err)
		}
		if err := renameMigrationWithoutReplace(workspace, destination); err != nil {
			return inventory, err
		}
		if err := durable.SyncDirectory(cfg.Storage.DataDir); err != nil {
			return inventory, errors.Join(ownedfile.ErrRetirement, err)
		}
		if err := haMetadataPreparePhase(phase, "prepare-source-preserved"); err != nil {
			return inventory, err
		}
	}
	// A successful Mkdir creates persistent bytes even if later preparation
	// fails. Discard retires only file creators, leaving this namespace opaque
	// for explicit restart; its directory charge therefore remains held.
	p.workspaceFee, err = manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{Disk: 4096})
	if err != nil {
		return inventory, err
	}
	if err := haMetadataPreparePhase(phase, "prepare-before-workspace-create"); err != nil {
		return inventory, errors.Join(err, p.workspaceFee.Release())
	}
	if err := os.Mkdir(workspace, 0700); err != nil {
		return inventory, errors.Join(err, p.workspaceFee.Release())
	}
	if err := durable.SyncDirectory(filepath.Dir(workspace)); err != nil {
		return inventory, errors.Join(ownedfile.ErrRetirement, err)
	}
	if err := haMetadataPreparePhase(phase, "prepare-workspace-created"); err != nil {
		return inventory, err
	}
	return inventory, nil
}
