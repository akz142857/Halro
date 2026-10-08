package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	bbolt "go.etcd.io/bbolt"
)

func dateReclaimFileFixture(t *testing.T, format string) (*diskFailureFixture, *Exporter, ManifestFile) {
	t.Helper()
	f, e := dateActivationFixture(t, format)
	files, err := e.ReadArchiveFiles(context.Background(), "", 64)
	if err != nil {
		t.Fatal(err)
	}
	var expired ManifestFile
	for _, entry := range files.Files {
		if entry.Date < "2026-10-05" {
			expired = entry
		}
	}
	if expired.Path == "" {
		t.Fatal("no expired fixture partition")
	}
	finishDateVerification(t, e, f.p, DefaultDateCatalogVerifyLimits())
	if _, err := e.ActivateRetainedDateCatalog(context.Background(), f.p, archiveDateCutoff); err != nil {
		t.Fatal(err)
	}
	return f, e, expired
}

func authorizeDateFileReclaim(t *testing.T, f *diskFailureFixture, e *Exporter) {
	t.Helper()
	finishPublishedDateVerification(t, e, f.p)
	finishDateReferences(t, e, DefaultDateCatalogReferenceLimits())
}

func TestArchiveDateFileReclaimCapsReadersAndMissingParent(t *testing.T) {
	for _, fault := range []string{"rows", "bytes", "reader", "missing-parent"} {
		t.Run(fault, func(t *testing.T) {
			f, e, entry := dateReclaimFileFixture(t, FormatNDJSON)
			authorizeDateFileReclaim(t, f, e)
			path := filepath.Join(e.root, filepath.FromSlash(entry.Path))
			limits := DefaultDateFileReclaimLimits()
			switch fault {
			case "rows":
				limits.FileRows = 1
			case "bytes":
				limits.FileBytes = 1
			case "reader":
				finish, err := e.beginArchiveRead(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer finish()
			case "missing-parent":
				if err := os.Rename(filepath.Dir(path), filepath.Dir(path)+".preserved"); err != nil {
					t.Fatal(err)
				}
			}
			out, err := e.ReclaimRetainedDateFile(context.Background(), f.p, archiveDateCutoff, entry, limits)
			if fault == "missing-parent" {
				if err != nil || !out.AlreadyMissing || out.Removed || out.RowsVerified != 0 || out.LogicalBytesUnlinked != 0 || !out.ObligationPending {
					t.Fatal(out, err)
				}
				if _, err := os.Stat(filepath.Join(filepath.Dir(path)+".preserved", filepath.Base(path))); err != nil {
					t.Fatal("missing name destroyed a foreign namespace", err)
				}
			} else {
				if err == nil || out != (DateFileReclamation{}) {
					t.Fatal(out, err)
				}
				if fault == "reader" && !errors.Is(err, ErrDiskUsageBusy) {
					t.Fatal(err)
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatal("cap/reader refusal deleted target", err)
				}
			}
		})
	}
}

func TestArchiveDateFileReclaimNativeExactBothFormatsAndMissingRetry(t *testing.T) {
	for _, format := range []string{FormatNDJSON, FormatParquet} {
		t.Run(format, func(t *testing.T) {
			f, e, entry := dateReclaimFileFixture(t, format)
			ctx := context.Background()
			nativePath := filepath.Join(filepath.Dir(f.p.db.Path()), "ledger.wal")
			nativeBefore, err := os.ReadFile(nativePath)
			if err != nil {
				t.Fatal(err)
			}
			rootBefore, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			authorizeDateFileReclaim(t, f, e)
			out, err := e.ReclaimRetainedDateFile(ctx, f.p, archiveDateCutoff, entry, DefaultDateFileReclaimLimits())
			if err != nil || !out.Removed || out.AlreadyMissing || !out.ObligationPending || out.RowsVerified != 2 || out.LogicalBytesUnlinked == 0 || out.Path != entry.Path || e.dateProof != nil || e.archiveEpoch != 2 {
				t.Fatal(out, err)
			}
			if _, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(entry.Path))); !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if out, err := e.ReclaimRetainedDateFile(ctx, f.p, archiveDateCutoff, entry, DefaultDateFileReclaimLimits()); !errors.Is(err, ErrArchiveDateProof) || out != (DateFileReclamation{}) {
				t.Fatal("consumed proof was reusable", out, err)
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			e, err = NewExporterWithOptions(e.root, datePolicyTestOptions(f, format))
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			authorizeDateFileReclaim(t, f, e)
			again, err := e.ReclaimRetainedDateFile(ctx, f.p, archiveDateCutoff, entry, DefaultDateFileReclaimLimits())
			if err != nil || again.Removed || !again.AlreadyMissing || !again.ObligationPending || again.RowsVerified != 0 || again.LogicalBytesUnlinked != 0 || e.archiveEpoch != 0 {
				t.Fatal(again, err)
			}
			rootAfter, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
			if err != nil || !bytes.Equal(rootBefore, rootAfter) {
				t.Fatal("data reclaim rewrote root", err)
			}
			nativeAfter, err := os.ReadFile(nativePath)
			if err != nil || !bytes.Equal(nativeBefore, nativeAfter) {
				t.Fatal("data reclaim changed billing facts", err)
			}
			state, _, marker, _, err := readDateActivationState(ctx, filepath.Join(e.root, archiveDateActivationName))
			if err != nil {
				t.Fatal(err)
			}
			if err := marker.Close(); err != nil {
				t.Fatal(err)
			}
			old, _, err := state.positions()
			if err != nil {
				t.Fatal(err)
			}
			for ref := old.CatalogHead; ref != (ArchivePageRef{}); {
				page, err := e.readCatalogPage(ctx, ref)
				if err != nil {
					t.Fatal("old pages were removed", err)
				}
				ref = page.Previous
			}
		})
	}
}

func TestArchiveDateFileReclaimRejectsContentScopeAndTargetOwners(t *testing.T) {
	for _, fault := range []string{"no-proof", "no-index", "scope", "checksum", "summary", "native", "inode", "parent-inode", "content-after", "symlink", "parent-symlink", "close", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			f, e, entry := dateReclaimFileFixture(t, FormatNDJSON)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := filepath.Join(e.root, filepath.FromSlash(entry.Path))
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if fault != "no-proof" {
				finishPublishedDateVerification(t, e, f.p)
			}
			if fault != "no-index" {
				finishDateReferences(t, e, DefaultDateCatalogReferenceLimits())
			}
			var hook func(*os.File) error
			switch fault {
			case "scope":
				entry.MaxSequence = entry.MaxSequence + 100
			case "checksum":
				entry.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
			case "summary":
				entry.InputTokens++
			case "native":
				if err := f.p.db.Update(func(tx *bbolt.Tx) error {
					a, found, err := diskUsageGet[AttemptEvent](tx, "attempts", diskUsageSequence(entry.MinSequence))
					if err != nil || !found {
						return errors.Join(err, ErrDiskUsageCorrupt)
					}
					a.ProviderInputTokens++
					return diskUsagePut(tx, "attempts", diskUsageSequence(entry.MinSequence), a)
				}); err != nil {
					t.Fatal(err)
				}
			case "inode":
				hook = func(*os.File) error {
					if err := os.WriteFile(path+".replacement", before, 0o600); err != nil {
						return err
					}
					return os.Rename(path+".replacement", path)
				}
			case "content-after":
				hook = func(*os.File) error { return os.WriteFile(path, append(before, ' '), 0o600) }
			case "parent-inode":
				hook = func(*os.File) error {
					parent := filepath.Dir(path)
					if err := os.Rename(parent, parent+".foreign"); err != nil {
						return err
					}
					if err := os.Mkdir(parent, 0o700); err != nil {
						return err
					}
					return os.WriteFile(path, before, 0o600)
				}
			case "symlink":
				if err := os.Rename(path, path+".foreign"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".foreign", path); err != nil {
					t.Fatal(err)
				}
			case "parent-symlink":
				parent := filepath.Dir(path)
				if err := os.Rename(parent, parent+".foreign"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(parent+".foreign", parent); err != nil {
					t.Fatal(err)
				}
			case "close":
				hook = func(f *os.File) error { return f.Close() }
			case "cancel":
				hook = func(*os.File) error { cancel(); return nil }
			}
			out, err := e.reclaimRetainedDateFile(ctx, f.p, archiveDateCutoff, entry, DefaultDateFileReclaimLimits(), hook, nil)
			if err == nil || out != (DateFileReclamation{}) {
				t.Fatal("invalid target/proof was reclaimed", out, err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("failed validation removed target", err)
			}
			if fault == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			spools, err := filepath.Glob(filepath.Join(e.root, ".archive-date-reclaim-spool-*"))
			if err != nil || len(spools) != 0 {
				t.Fatal("spool cleanup escaped", spools, err)
			}
		})
	}
}

func TestArchiveDateFileReclaimInterruptedUnlinkAndActualCloseJoin(t *testing.T) {
	for _, fault := range []string{"after-unlink", "actual-close-after-unlink", "before-cancel-join", "after-cancel-join"} {
		t.Run(fault, func(t *testing.T) {
			f, e, entry := dateReclaimFileFixture(t, FormatNDJSON)
			authorizeDateFileReclaim(t, f, e)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := filepath.Join(e.root, filepath.FromSlash(entry.Path))
			if fault == "after-unlink" || fault == "actual-close-after-unlink" {
				stop := errors.New("interrupted after actual unlink before parent sync")
				var held *os.File
				before := func(f *os.File) error { held = f; return nil }
				after := func() error {
					if fault == "actual-close-after-unlink" {
						return held.Close()
					}
					return stop
				}
				out, err := e.reclaimRetainedDateFile(ctx, f.p, archiveDateCutoff, entry, DefaultDateFileReclaimLimits(), before, after)
				if err == nil || fault == "after-unlink" && !errors.Is(err, stop) || out != (DateFileReclamation{}) {
					t.Fatal(out, err)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				authorizeDateFileReclaim(t, f, e)
				out, err = e.ReclaimRetainedDateFile(context.Background(), f.p, archiveDateCutoff, entry, DefaultDateFileReclaimLimits())
				if err != nil || !out.AlreadyMissing || !out.ObligationPending {
					t.Fatal(out, err)
				}
				return
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			var before func(*os.File) error
			var after func() error
			if fault == "before-cancel-join" {
				before = func(*os.File) error { close(entered); <-release; return nil }
			} else {
				after = func() error { close(entered); <-release; return nil }
			}
			done := make(chan error, 1)
			go func() {
				out, err := e.reclaimRetainedDateFile(ctx, f.p, archiveDateCutoff, entry, DefaultDateFileReclaimLimits(), before, after)
				if out != (DateFileReclamation{}) {
					err = errors.Join(err, errors.New("retired reclaim returned scope"))
				}
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("physical callback did not enter")
			}
			cancel()
			eClosed, pClosed := make(chan error, 1), make(chan error, 1)
			go func() { eClosed <- e.Close() }()
			go func() { pClosed <- f.p.Close() }()
			deadline := time.Now().Add(5 * time.Second)
			for !e.archiveRetired.Load() || !f.p.retired.Load() {
				if time.Now().After(deadline) {
					t.Fatal("owners did not withdraw")
				}
				time.Sleep(time.Millisecond)
			}
			select {
			case err := <-eClosed:
				t.Fatal("Exporter Close passed physical job", err)
			default:
			}
			select {
			case err := <-pClosed:
				t.Fatal("projection Close passed physical job", err)
			default:
			}
			unblock()
			if err := <-done; !errors.Is(err, context.Canceled) || !errors.Is(err, ErrDiskUsageClosed) {
				t.Fatal(err)
			}
			if err := <-eClosed; err != nil {
				t.Fatal(err)
			}
			if err := <-pClosed; err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(path)
			if fault == "before-cancel-join" && statErr != nil {
				t.Fatal("cancel before unlink removed target", statErr)
			}
			if fault == "after-cancel-join" && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal(statErr)
			}
			if _, err := os.Stat(filepath.Join(e.root, archiveDateActivationName)); err != nil {
				t.Fatal("late failure erased obligation", err)
			}
		})
	}
}

func TestArchiveDateFileReclaimReferencedHistoricalFilenameIsProtected(t *testing.T) {
	f, e, oldEntry := dateReclaimFileFixture(t, FormatNDJSON)
	ctx := context.Background()
	baseline := f.a.Watermark().Sequence
	for _, event := range windowRequest("late-reused", false, false) {
		appendDiskDetailEvent(t, f, event)
	}
	var late []AttemptEvent
	for _, a := range f.a.Snapshot().Attempts {
		if a.Sequence > baseline {
			late = append(late, a)
		}
	}
	entry := publishDateComparisonFault(t, e, oldEntry.Date, "reused", late)
	newPath := filepath.Join(e.root, filepath.FromSlash(entry.Path))
	oldPath := filepath.Join(e.root, filepath.FromSlash(oldEntry.Path))
	if err := os.Rename(newPath, oldPath); err != nil {
		t.Fatal(err)
	}
	entry.Path = oldEntry.Path
	root, err := openArchiveManifest(e.root)
	if err != nil {
		t.Fatal(err)
	}
	position, _, err := root.walk(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := archiveCatalogBuilder{exporter: e, ctx: ctx, head: position.CatalogHead, work: &archiveWork{remaining: 4 << 20}}
	if err := b.add(entry); err != nil {
		t.Fatal(err)
	}
	if err := b.flush(); err != nil {
		t.Fatal(err)
	}
	if err := e.publishArchiveCatalogRootWithRetention(ctx, root, false, f.a.Watermark().Sequence, false, b.head, position.Retention, &archiveWork{remaining: archiveManifestValueLimit}, nil); err != nil {
		t.Fatal(err)
	}
	if err := root.file.Close(); err != nil {
		t.Fatal(err)
	}
	authorizeDateFileReclaim(t, f, e)
	before, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.ReclaimRetainedDateFile(ctx, f.p, archiveDateCutoff, oldEntry, DefaultDateFileReclaimLimits())
	if !errors.Is(err, ErrArchiveDateReferenced) || out != (DateFileReclamation{}) {
		t.Fatal("old filename granted deletion", out, err)
	}
	after, err := os.ReadFile(oldPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("current reference was removed", err)
	}
	actual := sha256.Sum256(after)
	if hex.EncodeToString(actual[:]) != entry.SHA256 {
		t.Fatal("reused path lost its current contents")
	}
}
