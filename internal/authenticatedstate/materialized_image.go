package authenticatedstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// MaterializedImage owns only the fresh inode created by MaterializeImage.
// OpenFile transfers its original descriptor to a database; it never reopens
// saved bytes by name. The database must physically Close before Remove.
// Disk credit remains held through removal, including a failed removal.
type MaterializedImage struct {
	mu            sync.Mutex
	path          string
	file          *os.File
	owner         *ownedfile.Owner
	reservation   resourcebudget.Permit
	released      bool
	retirementErr error
}

// MaterializeImage authenticates every selected image chunk into an exclusive
// destination, Syncs it, and preserves its descriptor for the receiving engine.
// The caller holds the directory lock and supplies an independent selected root
// and a disk ceiling in this store's manager. Existing files are never replaced.
func (s *Store) MaterializeImage(ctx context.Context, path string, schema SealRootSchema, selected SealRoot, chunkBytes int, maxBytes int64, reservation resourcebudget.Permit) (result *MaterializedImage, resultErr error) {
	if s == nil || path == "" || maxBytes <= 0 {
		return nil, ErrLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	credit, err := reservation.RetainCovering(s.budget, resourcebudget.Cost{Disk: maxBytes})
	if err != nil {
		return nil, err
	}
	image := &MaterializedImage{reservation: credit}
	defer func() {
		if result == nil {
			if cleanupErr := image.Remove(); cleanupErr != nil {
				result, resultErr = image, errors.Join(resultErr, cleanupErr)
			}
		}
	}()
	image.path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	image.file, image.owner, err = ownedfile.Create(image.path, os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, ownedfile.ErrRetirement) {
			image.retirementErr = err
		}
		return nil, err
	}
	if _, err := s.WriteImage(ctx, image.file, schema, selected, chunkBytes, maxBytes); err != nil {
		return nil, err
	}
	if err := image.file.Sync(); err != nil {
		return nil, err
	}
	if err := image.owner.VerifyFile(image.file); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return image, nil
}

// OpenFile is a one-shot bbolt OpenFile callback. It transfers the exact
// descriptor just populated under authentication, without an open-by-name gap.
func (i *MaterializedImage) OpenFile(path string, flags int, _ os.FileMode) (*os.File, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	absolute, err := filepath.Abs(path)
	if err != nil || absolute != i.path || i.file == nil || i.released || flags&(os.O_TRUNC|os.O_EXCL|os.O_APPEND) != 0 {
		return nil, errors.Join(ErrRecovery, err)
	}
	if err := i.owner.VerifyFile(i.file); err != nil {
		return nil, err
	}
	file := i.file
	i.file = nil
	return file, nil
}

// EngineOpenFailed records an opaque receiving-engine construction failure.
// After descriptor transfer, a nil database cannot prove that its mmap and
// actual Close retired. Before transfer, Remove still owns the complete FD.
func (i *MaterializedImage) EngineOpenFailed(cause error) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if cause != nil && i.file == nil && !i.released {
		i.retirementErr = errors.Join(i.retirementErr, ownedfile.ErrRetirement, cause)
		return i.retirementErr
	}
	return cause
}

// Remove closes a descriptor not yet transferred, removes only the captured
// inode, then releases the retained disk credit. After transfer, the caller
// joins and closes the receiving engine first. Refusing a replaced path keeps
// the fee and permits retry after the original name has been restored.
func (i *MaterializedImage) Remove() error {
	if i == nil {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.released {
		return nil
	}
	if i.retirementErr != nil {
		return i.retirementErr
	}
	if i.file != nil {
		if err := i.file.Close(); err != nil {
			i.retirementErr = errors.Join(ownedfile.ErrRetirement, err)
			return i.retirementErr
		}
		i.file = nil
	}
	if err := i.owner.Retire(); err != nil {
		return err
	}
	if err := i.reservation.Release(); err != nil {
		return err
	}
	i.released = true
	return nil
}
