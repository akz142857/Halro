package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func copyDateOrphan(t *testing.T, e *Exporter, entry ManifestFile, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.root, filepath.FromSlash(entry.Path)))
	if err != nil {
		t.Fatal(err)
	}
	path := "date=" + entry.Date + "/" + name + filepath.Ext(entry.Path)
	if err := os.WriteFile(filepath.Join(e.root, filepath.FromSlash(path)), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestArchiveDateOrphanFileDerivesActualDescriptorBothFormats(t *testing.T) {
	for _, format := range []string{FormatNDJSON, FormatParquet} {
		t.Run(format, func(t *testing.T) {
			f, e, original := dateReclaimFileFixture(t, format)
			path := copyDateOrphan(t, e, original, "orphan-unlisted")
			policy, err := os.ReadFile(filepath.Join(e.root, archiveDateActivationName))
			if err != nil {
				t.Fatal(err)
			}
			root, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			authorizeDateFileReclaim(t, f, e)
			out, err := e.ReclaimRetainedDateOrphanFile(context.Background(), f.p, archiveDateCutoff, path, DefaultDateFileReclaimLimits())
			want := original
			want.Path = path
			if err != nil || out.Entry != want || !out.Reclamation.Removed || out.Reclamation.RowsVerified != int(original.Records) || !out.Reclamation.ObligationPending || out.Reclamation.LogicalBytesUnlinked <= 0 {
				t.Fatal(out, want, err)
			}
			if _, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(original.Path))); err != nil {
				t.Fatal("orphan operation deleted original descriptor path", err)
			}
			authorizeDateFileReclaim(t, f, e)
			again, err := e.ReclaimRetainedDateOrphanFile(context.Background(), f.p, archiveDateCutoff, path, DefaultDateFileReclaimLimits())
			if err != nil || again.Entry != (ManifestFile{}) || !again.Reclamation.AlreadyMissing || again.Reclamation.RowsVerified != 0 || again.Reclamation.LogicalBytesUnlinked != 0 {
				t.Fatal(again, err)
			}
			for name, before := range map[string][]byte{archiveDateActivationName: policy, "manifest.json": root} {
				after, err := os.ReadFile(filepath.Join(e.root, name))
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("active root/policy changed", name, err)
				}
			}
		})
	}
}

func TestArchiveDateOrphanFileRecreatedAfterOriginalPagesGone(t *testing.T) {
	for _, format := range []string{FormatNDJSON, FormatParquet} {
		t.Run(format, func(t *testing.T) {
			f, e, files := dateReclaimStepFixture(t, format, 2)
			path := filepath.Join(e.root, filepath.FromSlash(files[0].Path))
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			finishDateReclaimStep(t, f, e, DefaultDateReclaimStepLimits())
			authorizeDateReclaimStep(t, f, e)
			pages, err := e.ReclaimRetainedDatePagesStep(context.Background(), f.p, archiveDateCutoff, DefaultDatePageReclaimLimits())
			if err != nil || pages.Phase != "ready" || pages.PagesRemoved == 0 {
				t.Fatal(pages, err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			e = reopenDatePageFixture(t, f, e, format)
			out, err := e.ReclaimRetainedDateOrphanFile(context.Background(), f.p, archiveDateCutoff, files[0].Path, DefaultDateFileReclaimLimits())
			if err != nil || out.Entry != files[0] || !out.Reclamation.Removed || !out.Reclamation.ObligationPending {
				t.Fatal("required vanished catalog to identify recreated object", out, err)
			}
		})
	}
}

func TestArchiveDateOrphanFileRefusesFormatMismatchBothDirections(t *testing.T) {
	for _, format := range []string{FormatNDJSON, FormatParquet} {
		t.Run(format, func(t *testing.T) {
			f, e, original := dateReclaimFileFixture(t, format)
			path := copyDateOrphan(t, e, original, "wrong-container")
			ext := ".ndjson"
			if format == FormatNDJSON {
				ext = ".parquet"
			}
			wrong := strings.TrimSuffix(path, filepath.Ext(path)) + ext
			if err := os.Rename(filepath.Join(e.root, filepath.FromSlash(path)), filepath.Join(e.root, filepath.FromSlash(wrong))); err != nil {
				t.Fatal(err)
			}
			authorizeDateFileReclaim(t, f, e)
			out, err := e.ReclaimRetainedDateOrphanFile(context.Background(), f.p, archiveDateCutoff, wrong, DefaultDateFileReclaimLimits())
			if err == nil || out != (DateOrphanFileReclamation{}) {
				t.Fatal("physical format guessed from filename became authority", out, err)
			}
			if _, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(wrong))); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestArchiveDateOrphanFileRejectsUntrustedContentAndCaps(t *testing.T) {
	for _, fault := range []string{"native", "schema", "mixed-schema", "ordering", "empty", "malformed", "wrong-date", "rows", "bytes", "read", "index", "symlink", "parent-symlink", "no-proof", "no-index", "missing-parent"} {
		t.Run(fault, func(t *testing.T) {
			f, e, original := dateReclaimFileFixture(t, FormatNDJSON)
			path := copyDateOrphan(t, e, original, "orphan-validation")
			absolute := filepath.Join(e.root, filepath.FromSlash(path))
			limits := DefaultDateFileReclaimLimits()
			if fault != "no-proof" {
				finishPublishedDateVerification(t, e, f.p)
			}
			if fault != "no-index" {
				finishDateReferences(t, e, DefaultDateCatalogReferenceLimits())
			}
			switch fault {
			case "native", "schema", "mixed-schema", "ordering", "wrong-date":
				raw, err := os.ReadFile(absolute)
				if err != nil {
					t.Fatal(err)
				}
				var rows []parquetAttempt
				if err := archiveReadNDJSON(context.Background(), bytes.NewReader(raw), func(row parquetAttempt) error { rows = append(rows, row); return nil }); err != nil || len(rows) < 2 {
					t.Fatal(err, len(rows))
				}
				switch fault {
				case "native":
					rows[0].ProviderInputTokens++
				case "schema":
					rows[0].SchemaVersion = 100
				case "mixed-schema":
					rows[0] = narrowToSchema(rows[0], 3)
				case "ordering":
					rows[1].Sequence = rows[0].Sequence
				case "wrong-date":
					rows[0].CompletedAtMicros += int64(48 * time.Hour / time.Microsecond)
				}
				if err := writeNDJSONAtomic(context.Background(), absolute, rows, nil); err != nil {
					t.Fatal(err)
				}
			case "empty", "malformed":
				raw := []byte{}
				if fault == "malformed" {
					raw = []byte("{bad ndjson}")
				}
				if err := os.WriteFile(absolute, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "rows":
				limits.FileRows = 1
			case "bytes":
				limits.FileBytes = 1
			case "read":
				limits.ReadBytes = 2 << 20
				// A valid file still cannot bypass the aggregate physical read budget.
				if err := os.WriteFile(absolute, bytes.Repeat([]byte(" "), 2<<20), 0600); err != nil {
					t.Fatal(err)
				}
			case "index":
				limits.IndexIOBytes = 1 // Invalid bounded budget is refused before IO.
			case "symlink":
				if err := os.Rename(absolute, absolute+".foreign"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(absolute+".foreign", absolute); err != nil {
					t.Fatal(err)
				}
			case "parent-symlink":
				parent := filepath.Dir(absolute)
				if err := os.Rename(parent, parent+".foreign"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(parent+".foreign", parent); err != nil {
					t.Fatal(err)
				}
			case "missing-parent":
				if err := os.RemoveAll(filepath.Dir(absolute)); err != nil {
					t.Fatal(err)
				}
			}
			out, err := e.ReclaimRetainedDateOrphanFile(context.Background(), f.p, archiveDateCutoff, path, limits)
			if fault == "missing-parent" {
				if err != nil || !out.Reclamation.AlreadyMissing || out.Entry != (ManifestFile{}) {
					t.Fatal(out, err)
				}
				return
			}
			if err == nil || out != (DateOrphanFileReclamation{}) {
				t.Fatal("invalid orphan supplied permission", out, err)
			}
			if _, err := os.Lstat(absolute); err != nil {
				t.Fatal("refused orphan removed", err)
			}
			spools, err := filepath.Glob(filepath.Join(e.root, ".archive-date-reclaim-spool-*"))
			if err != nil || len(spools) != 0 {
				t.Fatal(spools, err)
			}
		})
	}
}

func TestArchiveDateOrphanFileLateNativeBeyondOriginalPolicyAndCurrentProtection(t *testing.T) {
	f, e, original := dateReclaimFileFixture(t, FormatNDJSON)
	originalHead := f.a.Watermark().Sequence
	for _, event := range windowRequest("late-orphan", false, false) {
		appendDiskDetailEvent(t, f, event)
	}
	var late []AttemptEvent
	for _, row := range f.a.Snapshot().Attempts {
		if row.Sequence > originalHead {
			late = append(late, row)
		}
	}
	// This object is genuine native content but outside the signed prune baseline.
	entry := publishDateComparisonFault(t, e, original.Date, "late-orphan", late)
	authorizeDateFileReclaim(t, f, e)
	out, err := e.ReclaimRetainedDateOrphanFile(context.Background(), f.p, archiveDateCutoff, entry.Path, DefaultDateFileReclaimLimits())
	if !errors.Is(err, ErrArchiveDateProof) || out != (DateOrphanFileReclamation{}) {
		t.Fatal(out, err)
	}
	if _, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(entry.Path))); err != nil {
		t.Fatal(err)
	}
	// The retained descriptor has its own independent date-scope fence.
	files, err := e.ReadArchiveFiles(context.Background(), "", 64)
	if err != nil || len(files.Files) == 0 {
		t.Fatal(files, err)
	}
	authorizeDateFileReclaim(t, f, e)
	out, err = e.ReclaimRetainedDateOrphanFile(context.Background(), f.p, archiveDateCutoff, files.Files[0].Path, DefaultDateFileReclaimLimits())
	if !errors.Is(err, ErrArchiveDateProof) || out != (DateOrphanFileReclamation{}) {
		t.Fatal(out, err)
	}
}

func TestArchiveDateOrphanFileProtectsCurrentExpiredNamedPath(t *testing.T) {
	f, e, old := dateReclaimFileFixture(t, FormatNDJSON)
	baseline := f.a.Watermark().Sequence
	for _, event := range windowRequest("late-current-orphan-path", false, false) {
		appendDiskDetailEvent(t, f, event)
	}
	var late []AttemptEvent
	for _, row := range f.a.Snapshot().Attempts {
		if row.Sequence > baseline {
			late = append(late, row)
		}
	}
	current := publishDateComparisonFault(t, e, old.Date, "current-orphan", late)
	if err := os.Rename(filepath.Join(e.root, filepath.FromSlash(current.Path)), filepath.Join(e.root, filepath.FromSlash(old.Path))); err != nil {
		t.Fatal(err)
	}
	current.Path = old.Path
	ctx := context.Background()
	root, err := openArchiveManifest(e.root)
	if err != nil {
		t.Fatal(err)
	}
	position, _, err := root.walk(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := archiveCatalogBuilder{exporter: e, ctx: ctx, head: position.CatalogHead, work: &archiveWork{remaining: 4 << 20}}
	if err := b.add(current); err != nil {
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
	out, err := e.ReclaimRetainedDateOrphanFile(ctx, f.p, archiveDateCutoff, old.Path, DefaultDateFileReclaimLimits())
	if !errors.Is(err, ErrArchiveDateReferenced) || out != (DateOrphanFileReclamation{}) {
		t.Fatal("current path was treated as orphan before source-scope decoding", out, err)
	}
	if _, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(old.Path))); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveDateOrphanFileRechecksTargetBeforeUnlink(t *testing.T) {
	for _, fault := range []string{"inode", "content", "close", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			f, e, original := dateReclaimFileFixture(t, FormatNDJSON)
			path := copyDateOrphan(t, e, original, "orphan-final-check")
			absolute := filepath.Join(e.root, filepath.FromSlash(path))
			raw, err := os.ReadFile(absolute)
			if err != nil {
				t.Fatal(err)
			}
			authorizeDateFileReclaim(t, f, e)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hook := func(file *os.File) error {
				switch fault {
				case "inode":
					if err := os.WriteFile(absolute+".replacement", raw, 0600); err != nil {
						return err
					}
					return os.Rename(absolute+".replacement", absolute)
				case "content":
					return os.WriteFile(absolute, append(raw, ' '), 0600)
				case "close":
					return file.Close()
				default:
					cancel()
					return nil
				}
			}
			out, err := e.reclaimRetainedDateOrphanFile(ctx, f.p, archiveDateCutoff, path, DefaultDateFileReclaimLimits(), hook, nil)
			if err == nil || out != (DateOrphanFileReclamation{}) {
				t.Fatal(out, err)
			}
			if _, err := os.Lstat(absolute); err != nil {
				t.Fatal("failed final target fence removed path", err)
			}
		})
	}
}

func TestArchiveDateOrphanFileLegacySchemaAndInvalidNames(t *testing.T) {
	f, e, original := dateReclaimFileFixture(t, FormatNDJSON)
	path := copyDateOrphan(t, e, original, "legacy-orphan")
	absolute := filepath.Join(e.root, filepath.FromSlash(path))
	raw, err := os.ReadFile(absolute)
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := archiveReadNDJSON(context.Background(), bytes.NewReader(raw), func(row parquetAttempt) error { return json.NewEncoder(&encoded).Encode(narrowToSchema(row, 3)) }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	authorizeDateFileReclaim(t, f, e)
	out, err := e.ReclaimRetainedDateOrphanFile(context.Background(), f.p, archiveDateCutoff, path, DefaultDateFileReclaimLimits())
	if err != nil || out.Entry.SchemaVersion != 3 || !out.Reclamation.Removed {
		t.Fatal(out, err)
	}
	for _, path := range []string{"../foreign.ndjson", "/date=2026-10-03/foo.ndjson", "date=2026-10-03/../foo.ndjson", "date=2026-10-03/nested/foo.ndjson", "date=2026-10-03/.spool.ndjson", "date=2026-10-03/foo.json", "date=2026-13-03/foo.ndjson", "date=2026-10-03/foo\\bar.ndjson", "manifest.json"} {
		if result, err := e.ReclaimRetainedDateOrphanFile(context.Background(), f.p, archiveDateCutoff, path, DefaultDateFileReclaimLimits()); err == nil || result != (DateOrphanFileReclamation{}) {
			t.Fatal(path, result, err)
		}
	}
}

func TestArchiveDateOrphanFileUnlinkFailureAndActualCloseJoin(t *testing.T) {
	f, e, original := dateReclaimFileFixture(t, FormatNDJSON)
	path := copyDateOrphan(t, e, original, "orphan-join")
	authorizeDateFileReclaim(t, f, e)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var out DateOrphanFileReclamation
	var jobErr error
	go func() {
		out, jobErr = e.reclaimRetainedDateOrphanFile(ctx, f.p, archiveDateCutoff, path, DefaultDateFileReclaimLimits(), nil, func() error { close(entered); <-release; return nil })
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("unlink not reached")
	}
	cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	closed := make(chan struct{})
	go func() { defer wg.Done(); _ = e.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned before actual physical call exit")
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	<-done
	wg.Wait()
	if !errors.Is(jobErr, context.Canceled) || out != (DateOrphanFileReclamation{}) {
		t.Fatal(out, jobErr)
	}
	if _, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(path))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("late error did not preserve committed unlink", err)
	}
	if e.datePolicyReady || e.datePolicyPrefix != nil || e.datePolicyKey != ([32]byte{}) {
		t.Fatal("Close did not erase independent policy key")
	}
}
