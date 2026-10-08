package app

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

const restoreReportRAMBytes int64 = 64 << 20
const restoreResultIDBytes = 8 << 20
const restoreReportCatalogBytes int64 = 32 << 20

var errRestoreReportRetirement = errors.New("restore report physical retirement is uncertain")

type restoreReportKey struct{}
type restoreReportJob struct {
	root    *commandResourceScope
	permit  resourcebudget.Permit
	debt    error
	catalog *backup.SourceCatalog
	count   int
}

func restoreReportContext(ctx context.Context, cfg config.Config) (context.Context, func() error, error) {
	ctx, pressure, err := commandBudgetContext(ctx, cfg)
	if err != nil {
		return ctx, nil, err
	}
	root, _ := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope)
	if old, ok := ctx.Value(restoreReportKey{}).(*restoreReportJob); ok {
		if old.root != root {
			return ctx, nil, resourcebudget.ErrInvalid
		}
		return ctx, func() error { return nil }, nil
	}
	job := &restoreReportJob{root: root}
	if cfg.ResourceBudget != nil {
		if pressure.logicalBudget() == nil {
			return ctx, nil, resourcebudget.ErrInvalid
		}
		job.permit, err = pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{RAM: restoreReportRAMBytes})
		if err != nil {
			return ctx, nil, err
		}
	}
	finish := func() error {
		if job.debt != nil {
			return job.debt
		}
		if job.permit == (resourcebudget.Permit{}) {
			return nil
		}
		err := job.permit.Release()
		if err == nil {
			job.permit = resourcebudget.Permit{}
		}
		return err
	}
	return context.WithValue(ctx, restoreReportKey{}, job), finish, nil
}
func restoreReportDebt(ctx context.Context, err error) {
	if err == nil {
		return
	}
	if job, ok := ctx.Value(restoreReportKey{}).(*restoreReportJob); ok {
		job.debt = errors.Join(job.debt, errRestoreReportRetirement, err)
	}
}
func restoreReportConstructorDebt(ctx context.Context, err error) {
	if errors.Is(err, ownedfile.ErrRetirement) || errors.Is(err, boltstore.ErrMetadataFileRetirement) || errors.Is(err, vault.ErrMasterKeyRetirement) || errors.Is(err, ledger.ErrSegmentRetirement) {
		restoreReportDebt(ctx, err)
	}
}

// A constructor may have failed after handing an actual descriptor to an
// engine. Record that debt before either publication or deferred cleanup.
func restoreReportReady(ctx context.Context, cause error) error {
	restoreReportConstructorDebt(ctx, cause)
	if job, ok := ctx.Value(restoreReportKey{}).(*restoreReportJob); ok {
		return job.debt
	}
	return nil
}

// These identities fence namespace replacement, not ownership of every child.
// The caller still excludes writers; complete creation receipts for the native
// files added during staged validation remain a separate requirement.
func retireRestoreStage(ctx context.Context, path string, identity, parentIdentity os.FileInfo, cause error) error {
	if err := restoreReportReady(ctx, cause); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	parent, parentErr := os.Lstat(filepath.Dir(path))
	if err != nil || parentErr != nil || identity == nil || parentIdentity == nil ||
		!os.SameFile(identity, current) || !os.SameFile(parentIdentity, parent) || !current.IsDir() || !parent.IsDir() {
		err = errors.Join(ownedfile.ErrRetirement, ownedfile.ErrReplaced, err, parentErr)
		restoreReportDebt(ctx, err)
		return err
	}
	err = os.RemoveAll(path)
	if err == nil {
		file, openErr := os.Open(filepath.Dir(path))
		err = openErr
		if openErr == nil {
			actual, statErr := file.Stat()
			if statErr != nil || !os.SameFile(parentIdentity, actual) {
				err = errors.Join(ownedfile.ErrReplaced, statErr)
			} else {
				err = file.Sync()
			}
			err = errors.Join(err, file.Close())
		}
	}
	restoreReportDebt(ctx, err)
	return err
}
func restoreReportClose(ctx context.Context, closer interface{ Close() error }) error {
	err := closer.Close()
	restoreReportDebt(ctx, err)
	return err
}

// RestoreBackupWithReport avoids retaining the full Gateway Key ID slice. The
// finite result and complete sorted walker are valid only inside consume. The
// same job owns candidate validation, catalog, publication and actual output.
func RestoreBackupWithReport(ctx context.Context, cfg config.Config, path string, key []byte, confirmation string, options RestoreOptions, consume func(RestoreResult, func(func(string) error) error) error) (resultErr error) {
	if consume == nil {
		return resourcebudget.ErrInvalid
	}
	ctx, finish, err := restoreReportContext(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, finish(), ctx.Err()) }()
	ctx, err = backupWorkspaceContext(ctx, cfg)
	if err != nil {
		return err
	}
	directory, err := os.MkdirTemp("", "halro-restore-report-*")
	if err != nil {
		return err
	}
	identity, err := os.Lstat(directory)
	if err != nil {
		restoreReportDebt(ctx, err)
		return err
	}
	defer func() {
		err := removeDoctorReferenceDirectory(directory, identity)
		restoreReportDebt(ctx, err)
		resultErr = errors.Join(resultErr, err)
	}()
	limit := restoreReportCatalogBytes
	if cfg.ResourceBudget != nil {
		limit = backupCatalogDiskCeiling(cfg)
	}
	catalog, err := backup.NewSourceCatalogWithContext(ctx, directory, limit)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, restoreReportClose(ctx, catalog)) }()
	job := ctx.Value(restoreReportKey{}).(*restoreReportJob)
	job.catalog = catalog
	result, err := RestoreBackupWithOptions(ctx, cfg, path, key, confirmation, options)
	if err != nil {
		return err
	}
	walk := func(consumeID func(string) error) error {
		if consumeID == nil {
			return resourcebudget.ErrInvalid
		}
		seen := 0
		err := catalog.Walk(ctx, func(source backup.SourceFile) error {
			if len(source.LocalPath) == 0 || len(source.LocalPath) > 4096 || source.ArchivePath != "keys/"+hex.EncodeToString([]byte(source.LocalPath)) {
				return boltstore.ErrVaultRewriteIncomplete
			}
			if seen >= job.count {
				return boltstore.ErrPricingRecoveryLimit
			}
			seen++
			return consumeID(source.LocalPath)
		})
		if err != nil {
			return err
		}
		if seen != job.count {
			return boltstore.ErrPricingRecoveryChanged
		}
		return ctx.Err()
	}
	return consume(result, walk)
}

// The compatibility API still returns every ID or fails before publication.
// It cannot allocate an unbounded legacy result. Default CLI uses the catalog.
func collectRestoreGatewayIDs(ctx context.Context, store *boltstore.Store) (ids []string, count int, resultErr error) {
	job, _ := ctx.Value(restoreReportKey{}).(*restoreReportJob)
	total := 0
	resultErr = store.WalkGatewayKeysMaintenance(ctx, func(item domain.GatewayKey) error {
		if !item.Enabled || item.DeletedAt != nil {
			return nil
		}
		if count == math.MaxInt {
			return boltstore.ErrPricingRecoveryLimit
		}
		if job != nil && job.catalog != nil {
			if err := job.catalog.Add(backup.SourceFile{ArchivePath: "keys/" + hex.EncodeToString([]byte(item.ID)), LocalPath: item.ID}); err != nil {
				return err
			}
		} else {
			size := len(item.ID) + 32
			if size > restoreResultIDBytes-total {
				return boltstore.ErrPricingRecoveryLimit
			}
			ids = append(ids, item.ID)
			total += size
		}
		count++
		return nil
	})
	if resultErr != nil {
		return nil, 0, resultErr
	}
	if job != nil && job.catalog != nil {
		job.count = count
		verified := 0
		resultErr = job.catalog.Walk(ctx, func(source backup.SourceFile) error {
			if verified >= count || len(source.LocalPath) == 0 || len(source.LocalPath) > 4096 || source.ArchivePath != "keys/"+hex.EncodeToString([]byte(source.LocalPath)) {
				return boltstore.ErrVaultRewriteIncomplete
			}
			verified++
			return nil
		})
		if resultErr == nil && verified != count {
			resultErr = boltstore.ErrPricingRecoveryChanged
		}
	}
	return
}

// WriteRestoreReportJSON preserves the existing complete JSON field contract.
// A closing object is emitted only after the complete authenticated walk and
// count have succeeded. A short actual writer cannot masquerade as success.
func WriteRestoreReportJSON(ctx context.Context, result RestoreResult, walk func(func(string) error) error, output io.Writer) error {
	if ctx == nil || walk == nil || output == nil || result.RestoredEnabledGatewayKeyCount < 0 {
		return resourcebudget.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, field := range []string{result.BackupID, result.DataDir, result.PreviousDataDir, result.UnlockPath, result.ClusterIncarnation, result.ClusterRole} {
		if len(field) > 4096 {
			return boltstore.ErrPricingRecoveryLimit
		}
	}
	result.RestoredEnabledGatewayKeyIDs = nil
	header, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if len(header) > 256<<10 {
		return boltstore.ErrPricingRecoveryLimit
	}
	marker := []byte(`"restored_enabled_gateway_key_ids":null`)
	at := bytes.Index(header, marker)
	if at < 0 {
		return boltstore.ErrVaultRewriteIncomplete
	}
	prefix := []byte(`"restored_enabled_gateway_key_ids":[`)
	write := func(value []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := output.Write(value)
		if err == nil && n != len(value) {
			err = io.ErrShortWrite
		}
		return errors.Join(err, ctx.Err())
	}
	if err := write(header[:at]); err != nil {
		return err
	}
	if err := write(prefix); err != nil {
		return err
	}
	seen := 0
	if err := walk(func(id string) error {
		if len(id) == 0 || len(id) > 4096 || seen >= result.RestoredEnabledGatewayKeyCount || seen == math.MaxInt {
			return boltstore.ErrPricingRecoveryLimit
		}
		raw, err := json.Marshal(id)
		if err != nil {
			return err
		}
		if seen > 0 {
			if err := write([]byte(",")); err != nil {
				return err
			}
		}
		if err := write(raw); err != nil {
			return err
		}
		seen++
		return nil
	}); err != nil {
		return err
	}
	if seen != result.RestoredEnabledGatewayKeyCount {
		return boltstore.ErrPricingRecoveryChanged
	}
	if err := write([]byte("]")); err != nil {
		return err
	}
	if err := write(header[at+len(marker):]); err != nil {
		return err
	}
	return write([]byte("\n"))
}
