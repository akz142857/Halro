package bolt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	bbolt "go.etcd.io/bbolt"
	bberrors "go.etcd.io/bbolt/errors"
)

var ErrMetadataStoreDiskLimit = errors.New("metadata database exceeds configured data-file ceiling")
var ErrMetadataFileRetirement = errors.New("Metadata physical close or retirement is uncertain")

// A data-file ceiling is a growth guard, not filesystem block reservation,
// native-journal authority, mmap/RSS accounting or permission to delete a DB.
func metadataFileOptions(options *bbolt.Options, maxBytes int64) (*bbolt.Options, error) {
	if maxBytes < 0 || maxBytes > int64(int(^uint(0)>>1)) {
		return nil, ErrMetadataStoreDiskLimit
	}
	copy := *options
	if maxBytes == 0 {
		return &copy, nil
	}
	copy.MaxSize = int(maxBytes)
	open := copy.OpenFile
	if open == nil {
		open = os.OpenFile
	}
	copy.OpenFile = func(path string, flags int, mode os.FileMode) (*os.File, error) {
		file, err := open(path, flags, mode)
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxBytes {
			closeErr := file.Close()
			if closeErr != nil {
				closeErr = errors.Join(ErrMetadataFileRetirement, closeErr)
			}
			return nil, errors.Join(err, closeErr, ErrMetadataStoreDiskLimit)
		}
		return file, nil
	}
	return &copy, nil
}

func openMetadataFile(path string, options *bbolt.Options, maxBytes int64) (*bbolt.DB, error) {
	db, _, err := openMetadataFileCaptured(path, options, maxBytes)
	return db, err
}

func openMetadataFileCaptured(path string, options *bbolt.Options, maxBytes int64) (*bbolt.DB, os.FileInfo, error) {
	bounded, err := metadataFileOptions(options, maxBytes)
	if err != nil {
		return nil, nil, err
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() {
		return nil, nil, errors.Join(err, errors.New("Metadata parent must be an existing real directory"))
	}
	open := bounded.OpenFile
	if open == nil {
		open = os.OpenFile
	}
	var actual os.FileInfo
	var opened *os.File
	handedToEngine := false
	bounded.OpenFile = func(name string, flags int, mode os.FileMode) (*os.File, error) {
		// A source engine hook may only open its actual source. Snapshot output
		// goes directly to a separate exclusive descriptor, never this hook.
		if filepath.Clean(name) != filepath.Clean(path) {
			return nil, errors.New("Metadata engine attempted to reopen a different file")
		}
		f, err := open(name, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK, mode)
		if err != nil {
			return nil, err
		}
		opened = f
		info, err := f.Stat()
		named, nameErr := os.Lstat(name)
		currentParent, parentErr := os.Lstat(filepath.Dir(name))
		if err != nil || nameErr != nil || parentErr != nil || !info.Mode().IsRegular() || !os.SameFile(info, named) || !os.SameFile(parent, currentParent) {
			closeErr := f.Close()
			if closeErr != nil {
				closeErr = errors.Join(ErrMetadataFileRetirement, closeErr)
			}
			return nil, errors.Join(err, nameErr, parentErr, closeErr, errors.New("Metadata actual file identity changed"))
		}
		actual = info
		handedToEngine = true
		return f, nil
	}
	db, err := bbolt.Open(path, 0600, bounded)
	if err != nil {
		// The engine returns no DB on Open failure and drops its cleanup error.
		// A closed FD alone cannot prove mmap/freelist retirement. Preserve the
		// mapping fee when the actual descriptor reached that opaque lifecycle.
		if handedToEngine {
			err = errors.Join(err, ErrMetadataFileRetirement)
		}
		// bbolt normally closes on constructor failure. Check the same actual
		// descriptor, not a newly opened name, before deciding its fee can retire.
		if opened != nil {
			if _, statErr := opened.Stat(); !errors.Is(statErr, os.ErrClosed) {
				if closeErr := opened.Close(); closeErr != nil {
					err = errors.Join(err, ErrMetadataFileRetirement, closeErr)
				}
			}
		}
		return nil, nil, normalizeMetadataFileError(err)
	}
	// A partial unpublished crypto rewrite is never a serving database. Its
	// temporary newer schema also makes older binaries refuse the same file.
	if err := db.View(func(tx *bbolt.Tx) error {
		if meta := tx.Bucket(bucketMeta); meta != nil && meta.Get(vaultRewritePendingKey) != nil {
			return ErrVaultRewriteIncomplete
		}
		return nil
	}); err != nil {
		return nil, nil, closeMetadataAfterError(db, err)
	}
	return db, actual, nil
}

func closeMetadataAfterError(db *bbolt.DB, cause error) error {
	if err := db.Close(); err != nil {
		return errors.Join(cause, ErrMetadataFileRetirement, err)
	}
	return cause
}

// The identity is captured from bbolt's actual OpenFile descriptor before
// mmap. A later name lookup can confirm it, but cannot create that authority.
func (s *Store) VerifyPhysicalIdentity(expected os.FileInfo) error {
	current, err := os.Lstat(s.path)
	if err != nil || expected == nil || s.physicalInfo == nil || !os.SameFile(expected, s.physicalInfo) || !os.SameFile(expected, current) {
		return errors.Join(err, errors.New("Metadata physical identity changed"))
	}
	return nil
}

func normalizeMetadataFileError(err error) error {
	if errors.Is(err, bberrors.ErrMaxSizeReached) {
		return errors.Join(ErrMetadataStoreDiskLimit, err)
	}
	return err
}

func OpenWithDiskLimit(path string, maxBytes int64) (*Store, error) {
	return openWithMigrationHooksLimit(path, nil, nil, maxBytes)
}

func OpenPrimaryWithDiskLimit(path string, maxBytes int64) (*Store, error) {
	store, err := openExactExistingMetadataLimit(path, nil, maxBytes)
	if err != nil {
		return nil, err
	}
	pending, err := store.definitionExportEpochPending()
	if err != nil || len(pending) != 0 {
		if err == nil {
			err = fmt.Errorf("%w: Primary metadata has an unpublished migration; complete offline preparation and the ordered schema boundary", ErrSchemaVersionMismatch)
		}
		return nil, errors.Join(err, store.Close())
	}
	return store, nil
}

func OpenReplicaWithDiskLimit(path string, maxBytes int64) (*Store, error) {
	return openReplicaLimit(path, maxBytes)
}

func OpenReadOnlyWithDiskLimit(path string, maxBytes int64) (*Store, error) {
	return openReadOnlyLimit(path, maxBytes)
}

func OpenForWholeFilePublishWithDiskLimit(path string, maxBytes int64) (*Store, error) {
	store, err := OpenWithDiskLimit(path, maxBytes)
	if err == nil {
		store.publishing = true
	}
	return store, err
}

func OpenExistingDerivedMaintenanceWithDiskLimit(path string, expected os.FileInfo, maxBytes int64) (*Store, error) {
	if expected == nil || !expected.Mode().IsRegular() {
		return nil, errors.New("derived maintenance requires authenticated Metadata identity")
	}
	return openExactExistingMetadataLimit(path, expected, maxBytes)
}
