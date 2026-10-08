package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func dateReferenceCatalogFixture(t *testing.T, count int) *Exporter {
	t.Helper()
	e := dateBuildCatalogFixture(t, count)
	build := finishDateCandidate(t, e, archiveDateCutoff)
	root, err := openArchiveManifest(e.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.publishArchiveCatalogRootWithRetention(context.Background(), root, false, uint64(count), false, build.Candidate, ArchiveDateRetention{Version: 1, RetainFromDate: "2026-10-05", PruneThroughSequence: uint64(count)}, &archiveWork{remaining: archiveManifestValueLimit}, nil); err != nil {
		t.Fatal(err)
	}
	if err := root.file.Close(); err != nil {
		t.Fatal(err)
	}
	return e
}

func finishDateReferences(t *testing.T, e *Exporter, limits DateCatalogReferenceLimits) DateCatalogReferenceProgress {
	t.Helper()
	for i := 0; i < 1024; i++ {
		out, err := e.PrepareDateCatalogReferencesStep(context.Background(), archiveDateCutoff, limits)
		if err != nil {
			t.Fatal(i, out, err)
		}
		if out.PagesProcessed > limits.Pages || out.PathsProcessed > limits.Pages*archiveCatalogPageFiles {
			t.Fatal("unbounded reference step", out)
		}
		if out.Phase == "ready" {
			return out
		}
	}
	t.Fatal("reference index stalled")
	return DateCatalogReferenceProgress{}
}

func referenceContains(t *testing.T, e *Exporter, tree int, path string) (found bool, err error) {
	t.Helper()
	err = e.withArchiveReclamation(context.Background(), func(advance func() error) (err error) {
		root, err := openArchiveManifest(e.root)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, root.file.Close()) }()
		found, err = e.dateReferenceContainsHeld(context.Background(), root, tree, path, &archiveVerifyReadBudget{remaining: 1 << 20})
		return err
	})
	return found, err
}

func TestArchiveDateReferencesTenThousandFinitePagesAndReadyReopen(t *testing.T) {
	e := dateReferenceCatalogFixture(t, 10000)
	ctx := context.Background()
	before, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	limits := DefaultDateCatalogReferenceLimits()
	limits.Pages = 1
	for want := uint64(1); want <= 3; want++ {
		out, err := e.PrepareDateCatalogReferencesStep(ctx, archiveDateCutoff, limits)
		if err != nil || out.PagesProcessed != 1 || out.PagesIndexed != want || out.Phase != "index" {
			t.Fatal(out, err)
		}
	}
	scratch := e.dateReferences.directory
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("scratch survived Close", err)
	}
	e, err = NewExporter(e.root)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for want := uint64(1); want <= 2; want++ {
		out, err := e.PrepareDateCatalogReferencesStep(ctx, archiveDateCutoff, limits)
		if err != nil || out.Phase != "revalidate" || out.ReplayPages != want || out.Root != (ArchivePosition{}) {
			t.Fatal("saved prefix was trusted", out, err)
		}
		if found, err := referenceContains(t, e, 2, "date=2026-10-05/usage-00000000000000000002.ndjson"); err == nil || found {
			t.Fatal("partial index supplied membership", found, err)
		}
	}
	out := finishDateReferences(t, e, limits)
	if out.FilesIndexed != 5000 || out.PagesIndexed != out.Root.CatalogHead.Pages || e.dateReferences.index.size > 2*int64(out.FilesIndexed+out.PagesIndexed+1)*archiveVerifyNodeBytes || e.archiveEpoch != 0 || e.windowProof != nil || e.dateProof != nil {
		t.Fatal(out)
	}
	for _, query := range []struct {
		tree int
		path string
		want bool
	}{
		{2, "date=2026-10-05/usage-00000000000000000002.ndjson", true},
		{2, "date=2026-10-03/usage-00000000000000000001.ndjson", false},
		{0, filepath.ToSlash(filepath.Join("catalog", "v1", "page-"+out.Root.CatalogHead.SHA256+".json")), true},
		{0, "catalog/v1/page-absent.json", false},
	} {
		found, err := referenceContains(t, e, query.tree, query.path)
		if err != nil || found != query.want {
			t.Fatal(query, found, err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = NewExporter(e.root)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	first, err := e.PrepareDateCatalogReferencesStep(ctx, archiveDateCutoff, limits)
	if err != nil || first.Phase != "revalidate" || first.PagesProcessed != 1 || first.Root != (ArchivePosition{}) {
		t.Fatal("saved Ready skipped history", first, err)
	}
	finishDateReferences(t, e, limits)
	after, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("reference preparation changed coverage", err)
	}
}

func TestArchiveDateReferenceSharedReservationFollowsScratchOwner(t *testing.T) {
	e := dateReferenceCatalogFixture(t, 512)
	requested, released := int64(0), 0
	e.archiveDiskReserve = func(_ context.Context, amount int64) (func() error, error) {
		requested += amount
		return func() error { released++; return nil }, nil
	}
	limits := DefaultDateCatalogReferenceLimits()
	limits.Pages = 1
	if out, err := e.PrepareDateCatalogReferencesStep(context.Background(), archiveDateCutoff, limits); err != nil || out.Phase != "index" {
		t.Fatal(out, err)
	}
	if requested != limits.IndexBytes || released != 0 || e.dateReferences == nil || e.dateReferences.releaseShared == nil {
		t.Fatal("shared reservation did not follow the retained index owner", requested, released)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if released != 1 {
		t.Fatal("shared reservation was not released after scratch cleanup", released)
	}
}

func TestArchiveDateReferencesSavedPrefixTamperAndExplicitReset(t *testing.T) {
	for _, fault := range []string{"next", "files", "root", "ready", "oversize", "symlink"} {
		t.Run(fault, func(t *testing.T) {
			e := dateReferenceCatalogFixture(t, 1000)
			limits := DefaultDateCatalogReferenceLimits()
			limits.Pages = 1
			for i := 0; i < 2; i++ {
				if _, err := e.PrepareDateCatalogReferencesStep(context.Background(), archiveDateCutoff, limits); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(e.root, archiveDateReferenceName)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var state archiveDateReferenceState
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "next":
				state.Next.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
			case "files":
				state.Files--
				state.Next.Files++
			case "root":
				state.RootSHA = "0000000000000000000000000000000000000000000000000000000000000000"
			case "ready":
				state.Pages = state.Root.CatalogHead.Pages
				state.Files = state.Root.Files
				state.Next = ArchivePageRef{}
			case "oversize":
				raw = bytes.Repeat([]byte("x"), archiveDateBuildBytes+1)
			case "symlink":
				if err := os.Rename(path, path+".foreign"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".foreign", path); err != nil {
					t.Fatal(err)
				}
			}
			if fault != "symlink" {
				if fault != "oversize" {
					raw, err = json.Marshal(state)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if fault == "ready" {
				first, err := e.PrepareDateCatalogReferencesStep(context.Background(), archiveDateCutoff, limits)
				if err != nil || first.Phase != "revalidate" || first.Root != (ArchivePosition{}) {
					t.Fatal(first, err)
				}
				finishDateReferences(t, e, limits) // recomputes every current reference
				return
			}
			refused := false
			for i := 0; i < 8; i++ {
				out, err := e.PrepareDateCatalogReferencesStep(context.Background(), archiveDateCutoff, limits)
				if err != nil {
					if out != (DateCatalogReferenceProgress{}) || e.dateReferences != nil {
						t.Fatal(out, err)
					}
					refused = true
					break
				}
				if out.Phase == "ready" {
					t.Fatal("tampered prefix became Ready", out)
				}
			}
			if !refused {
				t.Fatal("tamper was not rejected")
			}
			if fault == "symlink" {
				if err := e.ResetDateCatalogReferences(context.Background()); err == nil {
					t.Fatal("reset accepted foreign symlink")
				}
				return
			}
			if err := e.ResetDateCatalogReferences(context.Background()); err != nil {
				t.Fatal(err)
			}
			if e.dateReferences != nil {
				t.Fatal("reset kept private index")
			}
			finishDateReferences(t, e, limits)
		})
	}
}

func TestArchiveDateReferencesRootChangesAndIndexOwnershipWithdrawMembership(t *testing.T) {
	for _, fault := range []string{"append", "inode", "node", "index-file", "directory", "checkpoint", "noncanonical"} {
		t.Run(fault, func(t *testing.T) {
			e := dateReferenceCatalogFixture(t, 1000)
			out := finishDateReferences(t, e, DefaultDateCatalogReferenceLimits())
			query := filepath.ToSlash(filepath.Join("catalog", "v1", "page-"+out.Root.CatalogHead.SHA256+".json"))
			tree := 0
			switch fault {
			case "append":
				root, err := openArchiveManifest(e.root)
				if err != nil {
					t.Fatal(err)
				}
				if err := e.publishArchiveCatalogRootWithRetention(context.Background(), root, false, 2000, false, out.Root.CatalogHead, out.Root.Retention, &archiveWork{remaining: archiveManifestValueLimit}, nil); err != nil {
					t.Fatal(err)
				}
				if err := root.file.Close(); err != nil {
					t.Fatal(err)
				}
			case "inode":
				path := filepath.Join(e.root, "manifest.json")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+".replacement", raw, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path+".replacement", path); err != nil {
					t.Fatal(err)
				}
			case "node":
				if _, err := e.dateReferences.index.file.WriteAt([]byte{9}, archiveVerifyNodeBytes+24); err != nil {
					t.Fatal(err)
				}
			case "index-file":
				path := e.dateReferences.index.file.Name()
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+".replacement", raw, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path+".replacement", path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				path := e.dateReferences.directory
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "foreign"), []byte("preserve"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "checkpoint":
				path := filepath.Join(e.root, archiveDateReferenceName)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			case "noncanonical":
				tree = 2
				query = "date=2026-10-05/nested/../usage-00000000000000000002.ndjson"
			}
			if found, err := referenceContains(t, e, tree, query); err == nil || found {
				t.Fatal("stale index supplied membership", found, err)
			}
			if fault == "directory" {
				path := e.dateReferences.directory
				if err := e.Close(); err == nil {
					t.Fatal("foreign owner was accepted")
				}
				if _, err := os.Stat(filepath.Join(path, "foreign")); err != nil {
					t.Fatal("foreign owner was removed", err)
				}
			}
		})
	}
}

func TestArchiveDateReferencesCloseJoinsPhysicalIndexAndFailedClose(t *testing.T) {
	for _, fault := range []string{"cancel-join", "close", "checkpoint", "root"} {
		t.Run(fault, func(t *testing.T) {
			e := dateReferenceCatalogFixture(t, 1000)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			limits := DefaultDateCatalogReferenceLimits()
			limits.Pages = 1
			if _, err := e.PrepareDateCatalogReferencesStep(ctx, archiveDateCutoff, limits); err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan *os.File, 1), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			done := make(chan error, 1)
			go func() {
				out, err := e.prepareDateCatalogReferencesStep(ctx, archiveDateCutoff, limits, func() error {
					f := e.dateReferences.index.file
					if fault == "cancel-join" {
						entered <- f
						<-release
						return nil
					}
					if fault == "close" {
						return f.Close()
					}
					path := filepath.Join(e.root, archiveDateReferenceName)
					if fault == "root" {
						path = filepath.Join(e.root, "manifest.json")
					}
					raw, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					if err := os.WriteFile(path+".replacement", raw, 0o600); err != nil {
						return err
					}
					return os.Rename(path+".replacement", path)
				})
				if out != (DateCatalogReferenceProgress{}) {
					err = errors.Join(err, errors.New("failed reference step returned progress"))
				}
				done <- err
			}()
			if fault == "cancel-join" {
				var f *os.File
				select {
				case f = <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("index callback did not enter")
				}
				cancel()
				closed := make(chan error, 1)
				go func() { closed <- e.Close() }()
				deadline := time.Now().Add(5 * time.Second)
				for !e.archiveRetired.Load() {
					if time.Now().After(deadline) {
						t.Fatal("Close did not withdraw")
					}
					time.Sleep(time.Millisecond)
				}
				if _, err := f.Stat(); err != nil {
					t.Fatal("logical cancel closed owned FD", err)
				}
				select {
				case err := <-closed:
					t.Fatal("Close passed physical index job", err)
				default:
				}
				unblock()
				if err := <-done; !errors.Is(err, context.Canceled) || !errors.Is(err, ErrDiskUsageClosed) {
					t.Fatal(err)
				}
				if err := <-closed; err != nil {
					t.Fatal(err)
				}
				if _, err := f.Stat(); err == nil {
					t.Fatal("Close did not release index")
				}
			} else {
				if err := <-done; err == nil {
					t.Fatal("ownership/Close failure returned success")
				}
			}
			if e.dateReferences != nil {
				t.Fatal("failed step retained index")
			}
		})
	}
}

func TestArchiveDateReferencesExplicitPolicyLimitsAndDiskCap(t *testing.T) {
	e := dateReferenceCatalogFixture(t, 1000)
	for _, edit := range []func(*DateCatalogReferenceLimits){func(l *DateCatalogReferenceLimits) { l.Pages = 0 }, func(l *DateCatalogReferenceLimits) { l.ReadBytes = 1 }, func(l *DateCatalogReferenceLimits) { l.IndexIOBytes = 1 }, func(l *DateCatalogReferenceLimits) { l.IndexBytes = 0 }, func(l *DateCatalogReferenceLimits) { l.MaxDuration = 0 }} {
		limits := DefaultDateCatalogReferenceLimits()
		edit(&limits)
		if out, err := e.PrepareDateCatalogReferencesStep(context.Background(), archiveDateCutoff, limits); !errors.Is(err, ErrDiskUsageLimit) || out != (DateCatalogReferenceProgress{}) {
			t.Fatal(out, err)
		}
	}
	if out, err := e.PrepareDateCatalogReferencesStep(context.Background(), archiveDateCutoff.AddDate(0, 0, 1), DefaultDateCatalogReferenceLimits()); !errors.Is(err, ErrArchiveDateProof) || out != (DateCatalogReferenceProgress{}) {
		t.Fatal(out, err)
	}
	limits := DefaultDateCatalogReferenceLimits()
	limits.IndexBytes = 16 * archiveVerifyNodeBytes
	if out, err := e.PrepareDateCatalogReferencesStep(context.Background(), archiveDateCutoff, limits); !errors.Is(err, ErrDiskUsageLimit) || out != (DateCatalogReferenceProgress{}) || e.dateReferences != nil {
		t.Fatal(out, err)
	}
	limits = DefaultDateCatalogReferenceLimits()
	limits.IndexIOBytes = 16 << 20
	limits.ReadBytes = 2*archiveCatalogPageBytes + archiveDateBuildBytes
	limits.Pages = 16
	out, err := e.PrepareDateCatalogReferencesStep(context.Background(), archiveDateCutoff, limits)
	if err != nil || out.PagesProcessed < 1 || out.PagesProcessed >= 16 {
		t.Fatal("small work budget did not save finite progress", out, err)
	}
	finishDateReferences(t, e, limits)
}

func TestArchiveDateReferencesAndComparisonCloseBothOwnedIndexes(t *testing.T) {
	f, e := dateActivationFixture(t, FormatNDJSON)
	ctx := context.Background()
	finishDateVerification(t, e, f.p, DefaultDateCatalogVerifyLimits())
	if _, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff); err != nil {
		t.Fatal(err)
	}
	limits := DefaultDateCatalogVerifyLimits()
	limits.Source.Rows, limits.Source.PageRows = 1, 1
	compared, err := e.VerifyPublishedDateCatalogStep(ctx, f.p, archiveDateCutoff, limits)
	if err != nil || compared.Phase == "ready" || e.dateVerification == nil {
		t.Fatal(compared, err)
	}
	finishDateReferences(t, e, DefaultDateCatalogReferenceLimits())
	comparisonFD, referenceFD := e.dateVerification.index.file, e.dateReferences.index.file
	comparisonDir, referenceDir := e.dateVerification.directory, e.dateReferences.directory
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []*os.File{comparisonFD, referenceFD} {
		if _, err := f.Stat(); err == nil {
			t.Fatal("Close left owned index open")
		}
	}
	for _, path := range []string{comparisonDir, referenceDir} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("Close left scratch", path, err)
		}
	}
	if e.dateReferences != nil || e.dateVerification != nil {
		t.Fatal("Close kept owner references")
	}
}
