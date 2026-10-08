package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
)

// An existing common selection is recovered, never discarded or rebuilt from
// a pending header. Only a real seven-root/native/member full audit authorizes
// completion of the independently encrypted bootstrap's exclusive rename.
func resumeHAMemoryPublishedActivation(ctx context.Context, cfg config.Config, keys HAMetadataKeys, guard *lock.Lock, manager *resourcebudget.Manager, logger *slog.Logger) (a *HAMemoryRuntimeActivation, resultErr error) {
	final := haMemoryStateBootstrapPath(cfg)
	path := final
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		path += ".pending"
	} else if err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 {
		return nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	a, reopened, err := openHAMemoryActivationRead(ctx, cfg, keys, guard, manager, logger, path)
	if err != nil {
		return a, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, a.Close())
			resultErr = &HAMemoryActivationFailure{Owner: a, Cause: resultErr}
		}
	}()
	if err := verifyHAMemoryActivationUnlock(ctx, a, keys.Master, manager); err != nil {
		return a, err
	}
	if err := reopened.Close(); err != nil {
		return a, err
	}
	if err := statecut.WithHASelectedCut(ctx, a.owners, func(_ context.Context, cut statecut.HASelectedCut) error {
		r, err := a.owners.Components.Publisher.SelectedReceipt(cut.Authority)
		if err == nil {
			a.Receipt = r
		}
		return err
	}); err != nil {
		return a, err
	}
	if err := ctx.Err(); err != nil {
		return a, err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, current) || before.Size() != current.Size() || !before.ModTime().Equal(current.ModTime()) {
		return a, errors.Join(ownedfile.ErrReplaced, err)
	}
	if path != final {
		if err := renameMigrationWithoutReplace(path, final); err != nil {
			return a, err
		}
		if err := durable.SyncDirectory(filepath.Dir(final)); err != nil {
			return a, err
		}
		current, err := os.Lstat(final)
		if err != nil || !os.SameFile(before, current) {
			return a, errors.Join(ownedfile.ErrReplaced, err)
		}
	}
	a.BootstrapPath = final
	return a, nil
}
