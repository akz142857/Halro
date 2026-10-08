package metadatastate

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/store/lock"
)

// Read-only diagnostics replay the authenticated suffix into an exclusive
// disposable tree. Every row comes from the selected authenticated snapshot;
// the original pack, selector and journal remain opened read-only throughout.
// Neither the temporary tree nor its derived position may publish a common cut.
func (b *Baseline) cloneSelectedReadRoot(ctx context.Context, key []byte, o BaselineOptions) (resultErr error) {
	identity, err := b.store.RootIdentity(ctx)
	if err != nil {
		return err
	}
	pin, err := b.store.Snapshot(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, pin.Close()) }()
	b.readCloneParent, err = os.MkdirTemp("", "halro-metadata-read-")
	if err != nil {
		return err
	}
	b.readCloneInfo, err = os.Lstat(b.readCloneParent)
	if err != nil {
		return err
	}
	b.readSource, b.store = b.store, nil
	file, publication, err := ownedfile.Create(lock.PublicationPath(filepath.Join(b.readCloneParent, "root")), os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	b.readCloneLock = publication
	if err := ownedfile.CloseFile(file); err != nil {
		b.retirementErr = err
		return err
	}
	var page authenticatedstate.Page
	var offset int
	var count uint64
	cursor := ""
	b.store, err = authenticatedstate.CreateStore(ctx, filepath.Join(b.readCloneParent, "root"), authenticatedstate.StoreOptions{
		Scope: identity.Scope, ExpectedCut: identity.Cut, Key: key, Budget: o.Budget,
		MaxDiskBytes: o.MaxDiskBytes, MaxAppendBytes: o.MaxAppendBytes,
	}, func() (authenticatedstate.Row, bool, error) {
		if offset == len(page.Rows) {
			if page.Done {
				if count != identity.Rows {
					return authenticatedstate.Row{}, false, ErrSource
				}
				return authenticatedstate.Row{}, false, nil
			}
			var err error
			page, err = pin.Range(ctx, authenticatedstate.RangeOptions{Cursor: cursor, MaxRows: 64, MaxBytes: 8 << 20, MaxPages: 4096})
			if err != nil {
				return authenticatedstate.Row{}, false, err
			}
			cursor, offset = page.Cursor, 0
			if len(page.Rows) == 0 {
				if page.Done && count == identity.Rows {
					return authenticatedstate.Row{}, false, nil
				}
				return authenticatedstate.Row{}, false, ErrSource
			}
		}
		row := page.Rows[offset]
		offset++
		count++
		return row, true, ctx.Err()
	})
	return err
}

func (b *Baseline) removeSelectedReadRoot() error {
	if b.store != nil {
		if err := b.store.Discard(); err != nil {
			return errors.Join(ownedfile.ErrRetirement, err)
		}
	}
	if err := b.readCloneLock.Remove(); err != nil {
		return errors.Join(ownedfile.ErrRetirement, err)
	}
	current, err := os.Lstat(b.readCloneParent)
	if err != nil || b.readCloneInfo == nil || !os.SameFile(b.readCloneInfo, current) {
		return errors.Join(ownedfile.ErrRetirement, ownedfile.ErrReplaced, err)
	}
	if err := os.Remove(b.readCloneParent); err != nil {
		return errors.Join(ownedfile.ErrRetirement, err)
	}
	if err := durable.SyncDirectory(filepath.Dir(b.readCloneParent)); err != nil {
		return errors.Join(ownedfile.ErrRetirement, err)
	}
	b.readCloneParent = ""
	return nil
}
