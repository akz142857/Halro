package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/diskinventory"
	"github.com/akz142857/Halro/internal/store/lock"
)

// A failed actual FD retirement still has an owner reachable from the startup
// error, even though no Runtime may be returned to business readers.
type persistentInventoryFailure struct {
	error
	owner             *diskinventory.Baseline
	ledgerAppendOwner *runtimeLedgerAppendDiskOwner
}

func (e *persistentInventoryFailure) Unwrap() error { return e.error }

func preparePersistentInventory(ctx context.Context, cfg config.Config, guard *lock.Lock, pressure *resourcePressureRuntime) (*diskinventory.Baseline, error) {
	if cfg.ResourceBudget == nil || cfg.ResourceBudget.PersistentInventoryDiskBytes == 0 {
		return nil, nil
	}
	if guard == nil || pressure == nil || pressure.logicalBudget() == nil {
		return nil, errors.New("persistent inventory requires the actual shared Runtime resource owner and data lock")
	}
	root, err := filepath.Abs(cfg.Storage.DataDir)
	if err != nil {
		return nil, err
	}
	// An enabled data-tree baseline must cover every authoritative native file.
	// It cannot quietly charge one tree while allowing an outside native owner.
	for _, path := range []string{cfg.MetadataPath(), cfg.LedgerPath(), cfg.GovernancePath(), cfg.AuditPath(), cfg.ReplicationStatePath(), cfg.OrderingJournalPath()} {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(root, absolute)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return nil, fmt.Errorf("native path %q is outside configured persistent inventory tree", path)
		}
	}
	b, err := diskinventory.Reconcile(ctx, diskinventory.Options{DataDir: root, Guard: guard, Budget: pressure.logicalBudget(), Ceiling: cfg.ResourceBudget.PersistentInventoryDiskBytes, Deadline: time.Minute})
	if err == nil {
		err = b.Publish()
	}
	if err != nil {
		return nil, &persistentInventoryFailure{error: errors.Join(err, b.Close()), owner: b}
	}
	return b, nil
}
