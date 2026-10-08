package usage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

func diskExportFixture(t *testing.T, n int) *diskFailureFixture {
	t.Helper()
	var events []ledger.Event
	for i := 0; i < n; i++ {
		rows := diskFailureRequest(fmt.Sprintf("export_%d", i), "success", 1, false, true)
		for j := range rows {
			rows[j].OccurredAt = rows[j].OccurredAt.Add(time.Duration((i/3)%2) * 24 * time.Hour)
		}
		events = append(events, rows...)
	}
	return newDiskFailureFixture(t, events)
}

func TestDiskExportFormatsCompletePrefixAndLegacyCompatibility(t *testing.T) {
	for _, format := range []string{FormatParquet, FormatNDJSON} {
		t.Run(format, func(t *testing.T) {
			f := diskExportFixture(t, 14)
			e, err := NewExporterWithOptions(filepath.Join(t.TempDir(), "archive"), Options{Format: format})
			if err != nil {
				t.Fatal(err)
			}
			snapshot := f.a.Snapshot()
			// Upgrade an actual old schema catalog with a real old partition.
			early := snapshot
			early.Attempts = early.Attempts[:2]
			old, err := e.Export(early)
			if err != nil {
				t.Fatal(err)
			}
			old.SchemaVersion = 7
			for i := range old.Files {
				old.Files[i].SchemaVersion = 8
			}
			if err := e.commitManifest(old); err != nil {
				t.Fatal(err)
			}
			limits := DefaultDiskExportLimits()
			limits.Source.PageRows = 4
			cut, err := e.ExportDisk(context.Background(), f.p, limits)
			if err != nil || cut.Head != f.a.Watermark() || cut.Rows != len(snapshot.Attempts)-2 {
				t.Fatal(cut, err)
			}
			m, err := testPagedManifest(e)
			if err != nil || !m.CompletePrefix || m.LastSequence != cut.Head.Sequence || m.SchemaVersion != archiveCatalogSchema || len(m.Files) < 4 {
				t.Fatal(m, err)
			}
			if m.LastSequence <= m.Files[len(m.Files)-1].MaxSequence {
				t.Fatal("manifest lost trailing non-attempt facts")
			}
			verifyTestPagedRows(t, e, snapshot)
			position, err := e.ReadArchivePosition(context.Background())
			if err != nil || position.Files != uint64(len(m.Files)) || position.LastSequence != m.LastSequence {
				t.Fatal(position, err)
			}
			before, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			repeat, err := e.ExportDisk(context.Background(), f.p, limits)
			after, readErr := os.ReadFile(filepath.Join(e.root, "manifest.json"))
			if err != nil || readErr != nil || repeat.Rows != 0 || !reflect.DeepEqual(before, after) {
				t.Fatal("repeat rewrote or duplicated catalog", repeat, err, readErr)
			}
			// A later real native request, even with an older date, is appended once.
			for _, event := range diskFailureRequest("export_late", "success", 1, false, true) {
				appendDiskDetailEvent(t, f, event)
			}
			if _, err := e.ExportDisk(context.Background(), f.p, limits); err != nil {
				t.Fatal(err)
			}
			latest := f.a.Snapshot()
			verifyTestPagedRows(t, e, latest)
			report, err := e.PruneBefore(time.Now().AddDate(1, 0, 0))
			if !errors.Is(err, ErrArchivePagedCatalog) || report.RowsRemoved != 0 {
				t.Fatal(report, err)
			}
			// Unsupported legacy pruning preserves the paged frontier across reopen.
			reopened, err := NewExporter(e.root)
			if err != nil {
				t.Fatal(err)
			}
			position, err = reopened.ReadArchivePosition(context.Background())
			if err != nil || position.Files < 4 || position.LastSequence != f.a.Watermark().Sequence {
				t.Fatal(position, err)
			}
		})
	}
}

func TestDiskExportIncompletePrefixOrphansRetryAndConflict(t *testing.T) {
	for _, format := range []string{FormatParquet, FormatNDJSON} {
		t.Run(format, func(t *testing.T) {
			f := diskExportFixture(t, 7)
			e, err := NewExporterWithOptions(filepath.Join(t.TempDir(), "archive"), Options{Format: format})
			if err != nil {
				t.Fatal(err)
			}
			limits := DefaultDiskExportLimits()
			limits.Source.PageRows = 2
			limits.Source.Pages = 1
			cut, err := e.ExportDisk(context.Background(), f.p, limits)
			if !errors.Is(err, ErrDiskUsageLimit) || cut != (DiskArchiveCut{}) {
				t.Fatal(cut, err)
			}
			if _, err := e.ReadArchivePosition(context.Background()); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("incomplete source published a frontier", err)
			}
			orphans, err := filepath.Glob(filepath.Join(e.root, "date=*", "usage-bounded-v1-*"))
			if err != nil || len(orphans) != 1 {
				t.Fatal(orphans, err)
			}
			bytes, err := os.ReadFile(orphans[0])
			if err != nil {
				t.Fatal(err)
			}
			limits.Source.Pages = 4096
			if _, err := e.ExportDisk(context.Background(), f.p, limits); err != nil {
				t.Fatal("matching orphan was not reusable", err)
			}
			if err := e.VerifyArchiveFileChecksums(context.Background()); err != nil {
				t.Fatal(err)
			}
			// Remove only the derived manifest, then try a same-sized corrupted orphan.
			if err := os.Remove(filepath.Join(e.root, "manifest.json")); err != nil {
				t.Fatal(err)
			}
			bytes[len(bytes)/2] ^= 1
			if err := os.WriteFile(orphans[0], bytes, 0o600); err != nil {
				t.Fatal(err)
			}
			cut, err = e.ExportDisk(context.Background(), f.p, limits)
			if !errors.Is(err, ErrDiskUsageCorrupt) || cut != (DiskArchiveCut{}) {
				t.Fatal("conflicting orphan was credited", cut, err)
			}
			if _, err := e.ReadArchivePosition(context.Background()); !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			preserved, err := os.ReadFile(orphans[0])
			if err != nil || !reflect.DeepEqual(bytes, preserved) {
				t.Fatal("orphan was overwritten", err)
			}
		})
	}
}

func TestDiskExportEncoderAndIOBudgetsPreservePreviousManifest(t *testing.T) {
	f := diskExportFixture(t, 5)
	e, err := NewExporter(filepath.Join(t.TempDir(), "archive"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Export(Snapshot{}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*DiskExportLimits){func(l *DiskExportLimits) { l.PartitionBytes = 1 }, func(l *DiskExportLimits) { l.WriteBytes = 1 }, func(l *DiskExportLimits) { l.WorkBytes = 1 }, func(l *DiskExportLimits) { l.Source.Rows = 1 }, func(l *DiskExportLimits) { l.MaxDuration = 0 }} {
		limits := DefaultDiskExportLimits()
		edit(&limits)
		cut, err := e.ExportDisk(context.Background(), f.p, limits)
		if !errors.Is(err, ErrDiskUsageLimit) || cut != (DiskArchiveCut{}) {
			t.Fatal(cut, err)
		}
		after, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("failed job changed frontier", err)
		}
		if err := filepath.WalkDir(e.root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if strings.HasPrefix(d.Name(), ".archive-") {
				return fmt.Errorf("scratch survived: %s", path)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	e.mu.Lock()
	_, err = e.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits())
	e.mu.Unlock()
	if !errors.Is(err, ErrDiskUsageBusy) {
		t.Fatal("export queued behind another publisher", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.ExportDisk(ctx, f.p, DefaultDiskExportLimits()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestArchiveManifestBoundedValuesDuplicatesAndIdentity(t *testing.T) {
	for _, bad := range []string{
		`{"schema_version":8,"last_sequence":1,"files":[],"files":[]}`,
		`{"schema_version":8,"last_sequence":1,"files":[{},]}`,
		`{"schema_version":8,"last_sequence":1,"files":[],}`,
		`{"schema_version":8,"last_sequence":1,"files":[]} {}`,
		`{"schema_version":8,"files":[]}`,
		`{"schema_version":8,"last_sequence":1,"files":[],"extra":"` + strings.Repeat("x", archiveManifestValueLimit) + `"}`,
	} {
		if _, err := readArchiveManifest(context.Background(), strings.NewReader(bad), nil); err == nil {
			t.Fatal("invalid catalog accepted", bad[:min(90, len(bad))])
		}
	}
	root := t.TempDir()
	path := filepath.Join(root, "manifest.json")
	text := `{"files":null,"last_sequence":0,"schema_version":3}`
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := openArchiveManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer m.file.Close()
	pos, _, err := m.walk(context.Background(), nil)
	if err != nil || pos.SchemaVersion != 3 || pos.Files != 0 {
		t.Fatal(pos, err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.walk(context.Background(), nil); !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal("replacement mixed descriptors", err)
	}
}

func TestDiskArchiveCloseJoinsManifestPublicationLifetime(t *testing.T) {
	f := diskExportFixture(t, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		cut, err := f.p.streamArchive(context.Background(), 0, archiveTestLimits, func(context.Context, []AttemptEvent) error { return nil }, func(ctx context.Context, cut DiskArchiveCut) error {
			if f.p.db.Stats().OpenTxN != 0 || cut.Rows != 1 {
				return errors.New("publication held DB transaction or incomplete cut")
			}
			close(entered)
			<-release
			return ctx.Err()
		})
		if cut != (DiskArchiveCut{}) {
			err = errors.Join(err, errors.New("retired publication returned a cut"))
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not enter")
	}
	closed := make(chan error, 1)
	go func() { closed <- f.p.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for !f.p.retired.Load() {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("owner did not retire")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-closed:
		close(release)
		t.Fatal("Close escaped active publication", err)
	default:
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func TestDiskArchiveJobCloseJoinsPreparationAndCleanup(t *testing.T) {
	f := diskExportFixture(t, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	path := filepath.Join(t.TempDir(), "preparation.tmp")
	done := make(chan error, 1)
	go func() {
		_, err := f.p.archiveJob(context.Background(), archiveTestLimits, func(ctx context.Context) (cut DiskArchiveCut, err error) {
			file, err := os.Create(path)
			if err != nil {
				return cut, err
			}
			defer func() { err = errors.Join(err, file.Sync(), file.Close(), os.Remove(path)) }()
			if _, err := file.WriteString("physical catalog preparation"); err != nil {
				return cut, err
			}
			close(entered)
			<-release
			return cut, ctx.Err()
		})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("preparation did not enter")
	}
	closed := make(chan error, 1)
	go func() { closed <- f.p.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for !f.p.retired.Load() {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("owner did not retire")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-closed:
		close(release)
		t.Fatal("Close escaped preparation", err)
	default:
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("Close escaped scratch cleanup", err)
	}
}

func testPagedManifest(e *Exporter) (Manifest, error) {
	pos, err := e.ReadArchivePosition(context.Background())
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{SchemaVersion: pos.SchemaVersion, LastSequence: pos.LastSequence, CompletePrefix: pos.CompletePrefix}
	cursor := ""
	for i := 0; i < 10000; i++ {
		page, err := e.ReadArchiveFiles(context.Background(), cursor, 17)
		if err != nil {
			return Manifest{}, err
		}
		m.Files = append(m.Files, page.Files...)
		if page.NextCursor == "" {
			sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].MinSequence < m.Files[j].MinSequence })
			return m, nil
		}
		cursor = page.NextCursor
	}
	return Manifest{}, ErrDiskUsageLimit
}
func verifyTestPagedRows(t *testing.T, e *Exporter, snapshot Snapshot) {
	t.Helper()
	if err := e.VerifyArchiveFileChecksums(context.Background()); err != nil {
		t.Fatal(err)
	}
	m, err := testPagedManifest(e)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]parquetAttempt{}
	for _, a := range snapshot.Attempts {
		expected[a.EventID] = toParquetAttempt(a)
	}
	seen := map[string]bool{}
	for _, entry := range m.Files {
		rows, err := readAttemptRows(filepath.Join(e.root, filepath.FromSlash(entry.Path)), entry.format())
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(rows)) != entry.Records {
			t.Fatal("descriptor row count", entry)
		}
		for _, row := range rows {
			want, ok := expected[row.EventID]
			if !ok || seen[row.EventID] || row != narrowToSchema(want, row.SchemaVersion) {
				t.Fatal("catalog changed canonical row set", row.EventID)
			}
			seen[row.EventID] = true
		}
	}
	if len(seen) != len(expected) {
		t.Fatal("catalog lost rows", len(seen), len(expected))
	}
}
