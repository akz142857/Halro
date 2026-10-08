package usage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"

	"github.com/akz142857/Halro/internal/ledger"
)

// WalkArchiveBackupFiles streams current data/catalog references without a
// historical manifest slice. The caller retains offline directory exclusion
// through the later copying of these paths. A retained root requires its signed
// original obligation to be complete; interrupted reclamation is refused rather
// than silently dropping the old chain needed to finish it after restore.
func (e *Exporter) WalkArchiveBackupFiles(ctx context.Context, allowed ledger.Watermark, visit func(string) error) error {
	return e.walkArchiveBackupFiles(ctx, allowed, visit, nil)
}

// Private hook observes real file close failures without supplying any authority.
func (e *Exporter) walkArchiveBackupFiles(ctx context.Context, allowed ledger.Watermark, visit func(string) error, beforeClose func(*os.File) error) (err error) {
	// Bind cleanup to the named result before inner scopes declare their own
	// errors; a signed-control Close failure must withdraw the whole backup walk.
	joinClose := func(file *os.File) {
		if beforeClose != nil {
			err = errors.Join(err, beforeClose(file))
		}
		err = errors.Join(err, file.Close())
	}
	if visit == nil {
		return ErrDiskUsageLimit
	}
	finish, err := e.beginArchiveRead(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, finish(), ctx.Err()) }()
	if !e.mu.TryLock() {
		return ErrDiskUsageBusy
	}
	defer e.mu.Unlock()
	root, err := openArchiveManifest(e.root)
	if err != nil {
		return err
	}
	defer joinClose(root.file)
	position, _, err := root.walk(ctx, nil)
	if err != nil {
		return err
	}
	if position.Retention != (ArchiveDateRetention{}) {
		complete, err := e.readDateCompletion(ctx, position, allowed)
		if err != nil {
			return errors.Join(err, errors.New("backup requires completion of the published Usage date obligation"))
		}
		defer joinClose(complete.file)
		pages, _, file, info, err := readDatePagesState(ctx, filepath.Join(e.root, archiveDatePagesName))
		if err != nil {
			return err
		}
		defer joinClose(file)
		if err := e.verifyDatePages(pages); err != nil {
			return err
		}
		if !reflect.DeepEqual(pages, complete.state.OriginalChain) {
			return ErrArchiveDateProof
		}
		if err := checkDateBuildState(file, info, filepath.Join(e.root, archiveDatePagesName)); err != nil {
			return err
		}
		if err := complete.check(ctx); err != nil {
			return err
		}
		for _, name := range []string{archiveDateActivePolicyName, archiveDatePagesName} {
			if err := visit(name); err != nil {
				return err
			}
		}
		markerPath := filepath.Join(e.root, archiveDateActivationName)
		marker, _, markerFile, markerInfo, err := readDateActivationState(ctx, markerPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if markerFile != nil {
			defer joinClose(markerFile)
			if err := e.verifyDatePolicy(ctx, marker, allowed); err != nil {
				return err
			}
			_, candidate, err := marker.positions()
			if err != nil || candidate.Retention != position.Retention || candidate.LastSequence > position.LastSequence {
				return errors.Join(err, errors.New("backup requires completion of the pending Usage date policy"))
			}
			if err := checkDateBuildState(markerFile, markerInfo, markerPath); err != nil {
				return err
			}
			if err := visit(archiveDateActivationName); err != nil {
				return err
			}
		}
	}
	if err := visit("manifest.json"); err != nil {
		return err
	}
	if position.CatalogVersion == 1 {
		for ref := position.CatalogHead; ref != (ArchivePageRef{}); {
			page, err := e.readCatalogPage(ctx, ref)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(e.root, ref.path(e.root))
			if err != nil {
				return err
			}
			if err := visit(filepath.ToSlash(relative)); err != nil {
				return err
			}
			for _, entry := range page.Files {
				if err := visit(entry.Path); err != nil {
					return err
				}
			}
			ref = page.Previous
		}
	} else {
		if _, _, err := root.walk(ctx, func(entry ManifestFile) error { return visit(entry.Path) }); err != nil {
			return err
		}
	}
	return root.check()
}
