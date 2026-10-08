package app

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

var errBackupStagingDiskLimit = errors.New("backup staging tree capacity exhausted")

const backupStagingRAMBytes int64 = 64 << 10

// The separate fixed staging owner shares the immutable command Manager with
// every index/report/Metadata file. Published tree fees never refund merely
// because RemoveAll ran. One counter/token at a time, no historical inode map.
type backupStagingDiskOwner struct {
	quota         *runtimeLedgerAppendDiskOwner
	workRetired   bool
	metadataImage *metadatastate.MetadataImage
}

func prepareBackupStagingDisk(ctx context.Context, cfg config.Config, pressure *resourcePressureRuntime) (*backupStagingDiskOwner, error) {
	if cfg.ResourceBudget == nil {
		return nil, nil
	}
	shared, err := commandPressure(ctx, cfg, pressure)
	if err != nil {
		return nil, err
	}
	scope, ok := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope)
	limit := cfg.ResourceBudget.BackupWorkspaceDiskBytes
	if !ok || shared == nil || shared.logicalBudget() == nil || limit <= 0 || limit > 1<<50 {
		return nil, resourcebudget.ErrInvalid
	}
	scope.stagingMu.Lock()
	defer scope.stagingMu.Unlock()
	if scope.backupStaging != nil {
		return scope.backupStaging, nil
	}
	credit, err := shared.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{RAM: backupStagingRAMBytes, Disk: limit})
	if err != nil {
		return nil, err
	}
	owner := &backupStagingDiskOwner{quota: &runtimeLedgerAppendDiskOwner{credit: credit, ceiling: limit}}
	scope.backupStaging = owner
	return owner, nil
}

func (o *backupStagingDiskOwner) finish(cause error) error {
	if o == nil {
		return nil
	}
	o.quota.mu.Lock()
	defer o.quota.mu.Unlock()
	o.quota.closed = true
	if o.workRetired || errors.Is(cause, ownedfile.ErrRetirement) || errors.Is(cause, ledger.ErrSegmentRetirement) || errors.Is(cause, boltstore.ErrMetadataFileRetirement) {
		return nil
	}
	err := o.quota.credit.ReleasePart(resourcebudget.Cost{RAM: backupStagingRAMBytes})
	if err == nil {
		o.workRetired = true
	}
	return err
}

func (o *backupStagingDiskOwner) reserve(bytes int64) (ledger.NativeFileDiskReservation, error) {
	if o == nil {
		return nil, nil
	}
	token, err := o.quota.reserveNativeFile(bytes)
	if errors.Is(err, errLedgerAppendDiskLimit) {
		err = errors.Join(errBackupStagingDiskLimit, resourcebudget.ErrExhausted)
	}
	return token, err
}

func backupFileEnvelope(bytes int64) (int64, error) {
	const quantum int64 = 64 << 10
	if bytes < 0 || bytes > math.MaxInt64-quantum {
		return 0, errBackupStagingDiskLimit
	}
	return (bytes/quantum + 1) * quantum, nil
}

type backupStagingDebt struct {
	owner   *backupStagingDiskOwner
	receipt *ownedfile.Owner
	catalog *backup.SourceCatalog // Retains its actual DB on uncertain Close.
	cause   error
}

func (e *backupStagingDebt) Error() string { return e.cause.Error() }
func (e *backupStagingDebt) Unwrap() error { return e.cause }
func stagingDebt(owner *backupStagingDiskOwner, receipt *ownedfile.Owner, err error) error {
	if err == nil || owner == nil {
		return err
	}
	return &backupStagingDebt{owner: owner, receipt: receipt, cause: err}
}

func verifyBackupFileEnvelope(path string, receipt *ownedfile.Owner, limit int64) error {
	if err := receipt.VerifyName(); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Blocks < 0 || stat.Blocks > math.MaxInt64/512 || info.Size() < 0 || info.Size() > limit || stat.Blocks*512 > limit {
		return errBackupStagingDiskLimit
	}
	return nil
}

func snapshotBackupMetadata(ctx context.Context, cfg config.Config, store *boltstore.Store, path string, owner *backupStagingDiskOwner) (boltstore.MetadataInfo, error) {
	if store.AuthenticatedMetadata() {
		return snapshotSelectedBackupMetadata(ctx, cfg, store, path, owner)
	}
	if owner == nil {
		return snapshotCommandMetadata(ctx, cfg, store, path)
	}
	if _, err := reserveCommandMetadataFile(ctx, cfg, path); err != nil {
		return boltstore.MetadataInfo{}, err
	}
	source, err := os.Lstat(cfg.MetadataPath())
	if err != nil || !source.Mode().IsRegular() {
		return boltstore.MetadataInfo{}, errors.Join(err, ownedfile.ErrReplaced)
	}
	if err := store.VerifyPhysicalIdentity(source); err != nil {
		return boltstore.MetadataInfo{}, err
	}
	envelope, err := backupFileEnvelope(source.Size())
	if err != nil {
		return boltstore.MetadataInfo{}, err
	}
	token, err := owner.reserve(envelope)
	if err != nil {
		return boltstore.MetadataInfo{}, err
	}
	info, receipt, err := store.SnapshotCreateOwnedContext(ctx, path)
	if err == nil {
		err = errors.Join(durable.SyncDirectory(filepath.Dir(path)), verifyBackupFileEnvelope(path, receipt, envelope))
	}
	if err != nil {
		if receipt != nil && !errors.Is(err, ownedfile.ErrRetirement) && !errors.Is(err, boltstore.ErrMetadataFileRetirement) {
			retireErr := receipt.Retire()
			if retireErr == nil {
				retireErr = durable.SyncDirectory(filepath.Dir(path))
			}
			if retireErr == nil {
				retireErr = token.RetireKnown()
			}
			err = errors.Join(err, retireErr)
		}
		return boltstore.MetadataInfo{}, stagingDebt(owner, receipt, err)
	}
	return info, nil
}

type backupStagingWriter struct {
	ctx       context.Context
	writer    io.Writer
	remaining int64
}

func (w *backupStagingWriter) Write(raw []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(raw)) > w.remaining {
		return 0, errBackupStagingDiskLimit
	}
	n, err := w.writer.Write(raw)
	w.remaining -= int64(n)
	return n, err
}

func copyFileForBackupWithDisk(ctx context.Context, source, destination string, owner *backupStagingDiskOwner) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	input, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	var output *os.File
	var receipt *ownedfile.Owner
	var token ledger.NativeFileDiskReservation
	defer func() {
		closeErr := ownedfile.CloseFile(input)
		if output != nil {
			closeErr = errors.Join(closeErr, ownedfile.CloseFile(output))
		}
		if closeErr != nil {
			resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, closeErr)
		}
		if resultErr != nil && receipt != nil && !errors.Is(resultErr, ownedfile.ErrRetirement) {
			retireErr := receipt.Retire()
			if retireErr == nil {
				retireErr = durable.SyncDirectory(filepath.Dir(destination))
			}
			if retireErr == nil && token != nil {
				retireErr = token.RetireKnown()
			}
			resultErr = errors.Join(resultErr, retireErr)
		} else if resultErr == nil && receipt != nil {
			resultErr = errors.Join(durable.SyncDirectory(filepath.Dir(destination)), receipt.VerifyName())
		}
		resultErr = stagingDebt(owner, receipt, resultErr)
	}()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.Join(err, ownedfile.ErrReplaced)
	}
	envelope, err := backupFileEnvelope(info.Size())
	if err != nil {
		return err
	}
	if owner != nil {
		token, err = owner.reserve(envelope)
		if err != nil {
			return err
		}
	}
	output, receipt, err = ownedfile.Create(destination, os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	writer := &backupStagingWriter{ctx: ctx, writer: output, remaining: info.Size()}
	var buffer [32 << 10]byte
	count, err := io.CopyBuffer(writer, io.LimitReader(input, info.Size()), buffer[:])
	if err != nil || count != info.Size() {
		return errors.Join(err, io.ErrUnexpectedEOF)
	}
	actual, err := input.Stat()
	named, nameErr := os.Lstat(source)
	if err != nil || nameErr != nil || !os.SameFile(info, actual) || !os.SameFile(info, named) || actual.Size() != info.Size() || !actual.ModTime().Equal(info.ModTime()) {
		return errors.Join(err, nameErr, ownedfile.ErrReplaced)
	}
	if err := errors.Join(ctx.Err(), output.Sync(), receipt.VerifyFile(output)); err != nil {
		return err
	}
	return verifyBackupFileEnvelope(destination, receipt, envelope)
}
