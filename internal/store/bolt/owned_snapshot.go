package bolt

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	bbolt "go.etcd.io/bbolt"
	bberrors "go.etcd.io/bbolt/errors"
)

// The receipt always describes the actual exclusive creation, including failed
// copies. Callers retain it until actual Close and owned retirement are known.
func (s *Store) SnapshotCreateOwnedContext(ctx context.Context, path string) (result MetadataInfo, owner *ownedfile.Owner, resultErr error) {
	return s.snapshotCreateOwnedContext(ctx, path, func(name string) (*os.File, *ownedfile.Owner, error) {
		return ownedfile.Create(name, os.O_WRONLY, 0600)
	})
}

func (s *Store) snapshotCreateOwnedContext(ctx context.Context, path string, create func(string) (*os.File, *ownedfile.Owner, error)) (MetadataInfo, *ownedfile.Owner, error) {
	return s.snapshotCreateOwned(ctx, path, create, false)
}

func (s *Store) snapshotCreateOwned(ctx context.Context, path string, create func(string) (*os.File, *ownedfile.Owner, error), prepareParent bool) (result MetadataInfo, owner *ownedfile.Owner, resultErr error) {
	if s != nil && s.facade != nil {
		return result, nil, ErrFacadeMaintenance
	}
	if ctx == nil {
		return result, nil, errors.New("Metadata snapshot requires context")
	}
	if err := ctx.Err(); err != nil {
		return result, nil, err
	}
	var f *os.File
	defer func() {
		if f != nil {
			if err := ownedfile.CloseFile(f); err != nil {
				resultErr = errors.Join(resultErr, ErrMetadataFileRetirement, err)
			}
		}
		if resultErr != nil {
			result = MetadataInfo{}
		}
	}()
	resultErr = s.view(func(tx *Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.maxFileBytes > 0 && tx.tx.Size() > s.maxFileBytes {
			return ErrMetadataStoreDiskLimit
		}
		raw := tx.Bucket(bucketMeta).Get(keySchemaVersion)
		if len(raw) != 8 {
			return ErrSchemaVersionMismatch
		}
		result.SchemaVersion, result.TxID = binary.BigEndian.Uint64(raw), uint64(tx.ID())
		readerVersion := tx.Bucket(bucketMeta).Get(keyMinimumLedgerReaderVersion)
		if len(readerVersion) > 128 {
			return ErrPricingRecoveryLimit
		}
		result.MinimumLedgerReaderVersion = string(readerVersion)
		if epoch := tx.Bucket(bucketMeta).Get(keyLedgerFeatureEpoch); len(epoch) == 1 {
			result.LedgerFeatureEpoch = epoch[0]
		}
		result.MetadataJournalEpoch, _ = decodeUint64(tx.Bucket(bucketMeta).Get(keyMetadataJournalEpoch))
		result.MetadataJournalSequence, _ = decodeUint64(tx.Bucket(bucketMeta).Get(keyAppliedJournalSequence))
		if prepareParent {
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return err
			}
		}
		parent, err := os.Lstat(filepath.Dir(path))
		if err != nil || !parent.IsDir() {
			return errors.Join(err, errors.New("snapshot parent is not a real directory"))
		}
		f, owner, err = create(path)
		if err != nil {
			return err
		}
		// Never permit WriteTo's large source slices to become unbounded work.
		_, err = tx.tx.WriteTo(&metadataContextWriter{ctx: ctx, writer: f, remaining: tx.tx.Size()})
		if err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		if err := owner.VerifyFile(f); err != nil {
			return err
		}
		currentParent, err := os.Lstat(filepath.Dir(path))
		if err != nil || !os.SameFile(parent, currentParent) {
			return errors.Join(err, ownedfile.ErrReplaced)
		}
		return ctx.Err()
	})
	return result, owner, resultErr
}

type metadataContextWriter struct {
	ctx       context.Context
	writer    io.Writer
	remaining int64
}

func (w *metadataContextWriter) Write(raw []byte) (int, error) {
	if int64(len(raw)) > w.remaining {
		return 0, ErrMetadataStoreDiskLimit
	}
	n := 0
	for len(raw) != 0 {
		if err := w.ctx.Err(); err != nil {
			return n, err
		}
		size := min(len(raw), 32<<10)
		written, err := w.writer.Write(raw[:size])
		n += written
		w.remaining -= int64(written)
		if err != nil {
			return n, err
		}
		if written != size {
			return n, io.ErrShortWrite
		}
		raw = raw[size:]
	}
	return n, nil
}

func (s *Store) CompactCreateOwnedContext(ctx context.Context, path string) (*ownedfile.Owner, error) {
	return s.compactCreateOwned(ctx, path, false)
}

func (s *Store) compactCreateOwned(ctx context.Context, path string, prepareParent bool) (owner *ownedfile.Owner, resultErr error) {
	if s != nil && s.facade != nil {
		return nil, ErrFacadeMaintenance
	}
	if ctx == nil {
		return nil, errors.New("Metadata compaction requires context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	leave, err := s.enter()
	if err != nil {
		return nil, err
	}
	defer leave()
	var permit resourcebudget.Permit
	if s.maxFileBytes > 0 {
		if s.mappingBudget == nil {
			return nil, errors.New("bounded Metadata compaction requires original mapping manager")
		}
		permit, err = acquireMetadataMapping(s.mappingBudget, s.maxFileBytes)
		if err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, releaseMetadataMapping(permit))
	}
	if prepareParent {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, errors.Join(err, releaseMetadataMapping(permit))
		}
	}
	// maxFileBytes==0 is a legacy unmetered mapping; positive ceilings always
	// require the caller's original shared manager.
	options := &bbolt.Options{FreelistType: bbolt.FreelistMapType, OpenFile: func(name string, flags int, mode os.FileMode) (*os.File, error) {
		f, receipt, err := ownedfile.Create(name, flags, mode)
		if receipt != nil {
			owner = receipt
		}
		return f, err
	}}
	destination, err := openMetadataFile(path, options, s.maxFileBytes)
	if err != nil {
		if !errors.Is(err, ErrMetadataFileRetirement) && !errors.Is(err, ownedfile.ErrRetirement) {
			err = errors.Join(err, releaseMetadataMapping(permit))
		}
		return owner, err
	}
	defer func() {
		if err := destination.Close(); err != nil {
			resultErr = errors.Join(resultErr, ErrMetadataFileRetirement, err)
		} else {
			resultErr = errors.Join(resultErr, releaseMetadataMapping(permit))
		}
	}()
	resultErr = compactMetadataContext(ctx, destination, s.db)
	return owner, resultErr
}

// A fixed-depth source walk and <=8MiB destination transactions preserve one
// immutable source view, sequences and all classes, without retaining history.
func compactMetadataContext(ctx context.Context, destination, source *bbolt.DB) (resultErr error) {
	tx, err := destination.Begin(true)
	if err != nil {
		return err
	}
	defer func() {
		if tx != nil {
			err := tx.Rollback()
			if !errors.Is(err, bberrors.ErrTxClosed) {
				resultErr = errors.Join(resultErr, err)
			}
		}
	}()
	size := 0
	var path [32][]byte
	write := func(depth int, key, value []byte, sequence uint64, bucket bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > len(path) || bucket && depth == len(path) || len(key) > pricingRecoveryWideRowBytes || len(value) > pricingRecoveryWideRowBytes-len(key) {
			return ErrPricingRecoveryLimit
		}
		cost := len(key) + len(value)
		if cost == 0 {
			cost = 1
		}
		if size > pricingRecoveryWideRowBytes-cost {
			if err := tx.Commit(); err != nil {
				return normalizeMetadataFileError(err)
			}
			tx = nil
			tx, err = destination.Begin(true)
			if err != nil {
				return err
			}
			size = 0
		}
		size += cost
		var b *bbolt.Bucket
		if depth > 0 {
			b = tx.Bucket(path[0])
			for i := 1; i < depth && b != nil; i++ {
				b = b.Bucket(path[i])
			}
			if b == nil {
				return ErrVaultRewriteIncomplete
			}
			b.FillPercent = 1
		}
		if !bucket {
			return b.Put(key, value)
		}
		var created *bbolt.Bucket
		if depth == 0 {
			created, err = tx.CreateBucket(key)
		} else {
			created, err = b.CreateBucket(key)
		}
		if err != nil {
			return err
		}
		return created.SetSequence(sequence)
	}
	var walk func(*bbolt.Bucket, []byte, int) error
	walk = func(b *bbolt.Bucket, name []byte, depth int) error {
		if err := write(depth, name, nil, b.Sequence(), true); err != nil {
			return err
		}
		path[depth] = name
		defer func() { path[depth] = nil }()
		return b.ForEach(func(key, value []byte) error {
			if value == nil {
				return walk(b.Bucket(key), key, depth+1)
			}
			return write(depth+1, key, value, 0, false)
		})
	}
	if err := source.View(func(reader *bbolt.Tx) error {
		return reader.ForEach(func(name []byte, b *bbolt.Bucket) error { return walk(b, name, 0) })
	}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	resultErr = normalizeMetadataFileError(tx.Commit())
	tx = nil
	return resultErr
}

// Context variants retain the legacy successful-output contract while using
// actual creation receipts. A failed copy retires only its own known-closed
// output; uncertain Close, a missing name or a replacement retains the file.
func (s *Store) SnapshotContext(ctx context.Context, path string) (MetadataInfo, error) {
	info, owner, err := s.snapshotCreateOwned(ctx, path, func(name string) (*os.File, *ownedfile.Owner, error) {
		return ownedfile.Create(name, os.O_WRONLY, 0600)
	}, true)
	if err != nil {
		return MetadataInfo{}, retireFailedMetadataCopy(path, owner, err)
	}
	return info, nil
}

func (s *Store) CompactSnapshotContext(ctx context.Context, path string) error {
	owner, err := s.compactCreateOwned(ctx, path, true)
	return retireFailedMetadataCopy(path, owner, err)
}

func retireFailedMetadataCopy(path string, owner *ownedfile.Owner, cause error) error {
	if cause == nil || owner == nil || errors.Is(cause, ErrMetadataFileRetirement) || errors.Is(cause, ownedfile.ErrRetirement) {
		return cause
	}
	if err := owner.Retire(); err != nil {
		return errors.Join(cause, ErrMetadataFileRetirement, err)
	}
	if err := durable.SyncDirectory(filepath.Dir(path)); err != nil {
		return errors.Join(cause, ErrMetadataFileRetirement, err)
	}
	return cause
}
