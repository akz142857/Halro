package usage

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/klauspost/compress/snappy"
	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress"
	parquetSnappy "github.com/parquet-go/parquet-go/compress/snappy"
	parquetZstd "github.com/parquet-go/parquet-go/compress/zstd"
	"github.com/parquet-go/parquet-go/format"
)

func TestArchiveVerifyFitsActualSmallTreeAndCountsReservedIndexOnce(t *testing.T) {
	for _, format := range []string{FormatNDJSON, FormatParquet} {
		t.Run(format, func(t *testing.T) {
			f := diskExportFixture(t, 14)
			e, err := NewExporterWithOptions(t.TempDir(), Options{Format: format})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			ctx := context.Background()
			if _, err := e.ExportDisk(ctx, f.p, DefaultDiskExportLimits()); err != nil {
				t.Fatal(err)
			}
			stock, err := inventoryArchiveTree(ctx, e.root, 1<<30)
			if err != nil {
				t.Fatal(err)
			}
			// Default verification requests 512 MiB scratch. The entire actual
			// catalog plus bounded index/spool fits this much smaller tree.
			e.archiveDiskCeiling = stock + 128<<10
			var held int64
			e.archiveDiskReserve = func(_ context.Context, bytes int64) (func() error, error) {
				if bytes != 128<<10 || held != 0 {
					t.Fatal("scratch was not planned against actual remaining tree", bytes, held)
				}
				held = bytes
				return func() error {
					left, err := filepath.Glob(filepath.Join(e.root, ".archive-verify-*"))
					if err != nil || len(left) != 0 {
						t.Fatal("shared scratch fee released before actual retirement", left, err)
					}
					held = 0
					return nil
				}, nil
			}
			for i := 0; i < 2; i++ {
				out, err := e.VerifyDiskArchive(ctx, f.p, DefaultArchiveVerifyLimits())
				if err != nil || out.ArchiveRows != 14 || out.SourceRows != 14 || held != 0 {
					t.Fatal("small catalog did not verify exactly", out, held, err)
				}
			}
			e.archiveDiskCeiling = stock + 2*archiveVerifyNodeBytes - 1
			if out, err := e.VerifyDiskArchive(ctx, f.p, DefaultArchiveVerifyLimits()); !errors.Is(err, ErrDiskUsageLimit) || out != (ArchiveVerification{}) || e.windowProof != nil || held != 0 {
				t.Fatal("insufficient actual scratch published proof", out, held, err)
			}
		})
	}
}

func TestArchiveVerifyExactRowsAgainstRealNativeAndFrozenPublishedPrefix(t *testing.T) {
	for _, container := range []string{FormatParquet, FormatNDJSON} {
		t.Run(container, func(t *testing.T) {
			f := diskExportFixture(t, 14)
			e, err := NewExporterWithOptions(t.TempDir(), Options{Format: container})
			if err != nil {
				t.Fatal(err)
			}
			cut, err := e.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits())
			if err != nil {
				t.Fatal(err)
			}
			// The native reference can advance; verification remains at archive H.
			for _, event := range diskFailureRequest("verify_future", "success", 1, false, true) {
				appendDiskDetailEvent(t, f, event)
			}
			report, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits())
			if err != nil || report.ArchiveRows != 14 || report.SourceRows != 14 || report.ThroughSequence != cut.Head.Sequence || report.SourceHead != f.a.Watermark() || report.SourceHead.Sequence <= report.ThroughSequence {
				t.Fatal(report, err)
			}
			leftover, err := filepath.Glob(filepath.Join(e.root, ".archive-verify-*"))
			if err != nil || len(leftover) != 0 {
				t.Fatal("verification scratch survived", leftover, err)
			}
		})
	}
}

func rewriteTestVerificationArchive(t *testing.T, e *Exporter, rows []parquetAttempt, mutateEntry func(*ManifestFile)) {
	t.Helper()
	var entries []ManifestFile
	for start := 0; start < len(rows); {
		date := time.UnixMicro(rows[start].CompletedAtMicros).UTC().Format("2006-01-02")
		end := start + 1
		for end < len(rows) && time.UnixMicro(rows[end].CompletedAtMicros).UTC().Format("2006-01-02") == date {
			end++
		}
		relative := filepath.ToSlash(filepath.Join("date="+date, fmt.Sprintf("test-%d.ndjson", start)))
		path := filepath.Join(e.root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeNDJSONAtomic(context.Background(), path, rows[start:end], nil); err != nil {
			t.Fatal(err)
		}
		checksum, err := fileSHA256(path)
		if err != nil {
			t.Fatal(err)
		}
		entry := ManifestFile{SchemaVersion: 8, Path: relative, Date: date, SHA256: checksum, MinSequence: uint64(rows[start].Sequence), MaxSequence: uint64(rows[end-1].Sequence), Records: int64(end - start), Format: FormatNDJSON}
		for _, row := range rows[start:end] {
			entry.InputTokens += row.ProviderInputTokens
			entry.OutputTokens += row.ProviderOutputTokens
			entry.CostMicrosUSD += row.CostMicrosUSD
		}
		if mutateEntry != nil {
			mutateEntry(&entry)
		}
		entries = append(entries, entry)
		start = end
	}
	old, err := openArchiveManifest(e.root)
	if err != nil {
		t.Fatal(err)
	}
	defer old.file.Close()
	position, _, err := old.walk(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	work := &archiveWork{remaining: 1 << 20}
	builder := archiveCatalogBuilder{exporter: e, ctx: context.Background(), work: work}
	for _, entry := range entries {
		if err := builder.add(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := builder.flush(); err != nil {
		t.Fatal(err)
	}
	if err := e.publishArchiveCatalogRoot(context.Background(), old, false, position.LastSequence, true, builder.head, work); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveVerifyDetectsCanonicalCorruptionDespiteValidChecksums(t *testing.T) {
	cases := []string{"content", "missing_middle", "duplicate_id", "duplicate_sequence", "extra", "summary", "schema", "missing_prefix", "missing_suffix"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			f := diskExportFixture(t, 5)
			e, err := NewExporterWithOptions(t.TempDir(), Options{Format: FormatNDJSON})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits()); err != nil {
				t.Fatal(err)
			}
			rows := make([]parquetAttempt, 0, 5)
			for _, attempt := range f.a.Snapshot().Attempts {
				rows = append(rows, toParquetAttempt(attempt))
			}
			var mutate func(*ManifestFile)
			switch name {
			case "content":
				rows[1].RequestedModel = "same_checksum_wrong_content"
			case "missing_prefix":
				rows = rows[1:]
			case "missing_suffix":
				rows = rows[:len(rows)-1]
			case "missing_middle":
				rows = append(rows[:2], rows[3:]...)
			case "duplicate_id":
				rows[1].EventID = rows[0].EventID
			case "duplicate_sequence":
				rows[1].Sequence = rows[0].Sequence
			case "extra":
				rows[1].EventID = "unknown_native_event"
			case "summary":
				mutate = func(e *ManifestFile) { e.InputTokens++ }
			case "schema":
				mutate = func(e *ManifestFile) { e.SchemaVersion = 7 }
			}
			rewriteTestVerificationArchive(t, e, rows, mutate)
			if err := e.VerifyArchiveFileChecksums(context.Background()); err != nil {
				t.Fatal("fixture must retain valid content hashes", err)
			}
			report, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits())
			if err == nil || report != (ArchiveVerification{}) {
				t.Fatal("corrupt exact set credited", name, report, err)
			}
		})
	}
}

func TestArchiveVerifyBudgetsCancellationAndClosedOwnerNeverCredit(t *testing.T) {
	f := diskExportFixture(t, 5)
	e, err := NewExporter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits()); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*ArchiveVerifyLimits){func(l *ArchiveVerifyLimits) { l.Files = 1 }, func(l *ArchiveVerifyLimits) { l.Rows = 1 }, func(l *ArchiveVerifyLimits) { l.FileBytes = 1 }, func(l *ArchiveVerifyLimits) { l.ReadBytes = 1 }, func(l *ArchiveVerifyLimits) { l.IndexBytes = 1 }, func(l *ArchiveVerifyLimits) { l.Source.Rows = 1 }, func(l *ArchiveVerifyLimits) { l.MaxDuration = 0 }} {
		limits := DefaultArchiveVerifyLimits()
		edit(&limits)
		report, err := e.VerifyDiskArchive(context.Background(), f.p, limits)
		if !errors.Is(err, ErrDiskUsageLimit) || report != (ArchiveVerification{}) {
			t.Fatal(report, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if report, err := e.VerifyDiskArchive(ctx, f.p, DefaultArchiveVerifyLimits()); !errors.Is(err, context.Canceled) || report != (ArchiveVerification{}) {
		t.Fatal(report, err)
	}
	if err := f.p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits()); !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
}

func TestArchiveVerifyLegacySchemaNarrowingAndExplicitRetainedFloor(t *testing.T) {
	for schema := 3; schema <= 8; schema++ {
		t.Run(string(rune('0'+schema)), func(t *testing.T) {
			f := diskExportFixture(t, 5)
			e, err := NewExporterWithOptions(t.TempDir(), Options{Format: FormatNDJSON})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits()); err != nil {
				t.Fatal(err)
			}
			attempts := f.a.Snapshot().Attempts
			rows := make([]parquetAttempt, 0, 3)
			for _, a := range attempts[2:] {
				rows = append(rows, narrowToSchema(toParquetAttempt(a), int32(schema)))
			}
			rewriteTestVerificationArchive(t, e, rows, func(entry *ManifestFile) { entry.SchemaVersion = schema })
			report, err := e.VerifyDiskArchiveAfter(context.Background(), f.p, attempts[2].Sequence-1, DefaultArchiveVerifyLimits())
			if err != nil || report.AfterSequence != attempts[2].Sequence-1 || report.FirstRetainedSequence != attempts[2].Sequence || report.SourceRows != 3 || report.ArchiveRows != 3 {
				t.Fatal(report, err)
			}
		})
	}
}

func TestArchiveVerifyDecoderPreflightRejectsAmplification(t *testing.T) {
	// Compact list length is checked before the library's typed allocation.
	raw := []byte{0x19, 0xfc, 0xff, 0xff, 0xff, 0x7f, 0}
	if _, err := archiveThriftFrame(raw, 1024); err == nil {
		t.Fatal("unbounded declared list accepted")
	}
	decoded := snappy.Encode(nil, []byte(strings.Repeat("x", 100)))
	header := format.PageHeader{UncompressedPageSize: 1, CompressedPageSize: int32(len(decoded))}
	if err := archiveValidateCompressedPage(format.Snappy, header, decoded); err == nil {
		t.Fatal("false uncompressed size accepted")
	}
	path := filepath.Join(t.TempDir(), "footer.parquet")
	tail := make([]byte, 12)
	copy(tail, "PAR1")
	binary.LittleEndian.PutUint32(tail[4:8], 1<<30)
	copy(tail[8:], "PAR1")
	if err := os.WriteFile(path, tail, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := archiveParquetPreflight(context.Background(), file, int64(len(tail)), &archiveVerifyReadBudget{remaining: 1 << 20}); !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal(err)
	}
	// The staged spool verifies bytes before any Parquet decoder sees them.
	e, err := NewExporter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entry := syntheticArchiveEntry(1)
	dir := filepath.Join(e.root, "date=2026-10-03")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte("untrusted")
	if err := os.WriteFile(filepath.Join(e.root, entry.Path), data, 0o600); err != nil {
		t.Fatal(err)
	}
	entry.SHA256 = strings.Repeat("0", 64)
	budget := &archiveVerifyReadBudget{remaining: 1024}
	if _, err := copyArchiveVerifySpool(context.Background(), e, entry, t.TempDir(), 1024, budget); !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal(err)
	}
}

// Real files omit columns that did not exist in their historical schema. This
// tests reader conversion, not merely the version number on a current schema.
func TestArchiveVerifyHistoricalParquetColumnsAndCodecs(t *testing.T) {
	introduced := map[string]int{"ProviderCachedInputTokens": 4, "ProviderCacheWriteInputTokens": 4, "ProviderReasoningTokens": 4, "ProviderCode": 5, "ProviderRequestID": 5, "FailurePhase": 5, "WorkUnitID": 6, "RunID": 6, "OfferingID": 7, "ProfileID": 7, "AccountRegionID": 8, "ProviderFailureReason": 8, "Retryable": 8, "Ambiguous": 8, "FailureSemanticsRecorded": 8}
	for schema := 3; schema <= 8; schema++ {
		for _, codec := range []compress.Codec{&parquetSnappy.Codec{}, &parquetZstd.Codec{}} {
			t.Run(fmt.Sprintf("schema%d/%s", schema, codec), func(t *testing.T) {
				f := diskExportFixture(t, 2)
				e, err := NewExporter(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				original := reflect.TypeOf(parquetAttempt{})
				var fields []reflect.StructField
				for i := 0; i < original.NumField(); i++ {
					field := original.Field(i)
					if introduced[field.Name] <= schema {
						fields = append(fields, field)
					}
				}
				oldType := reflect.StructOf(fields)
				path := filepath.Join(e.root, "date=2026-10-03", "legacy.parquet")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				file, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				writer := parquet.NewWriter(file, parquet.SchemaOf(reflect.New(oldType).Elem().Interface()), parquet.Compression(codec))
				attempts := f.a.Snapshot().Attempts
				for _, a := range attempts {
					row := reflect.ValueOf(narrowToSchema(toParquetAttempt(a), int32(schema)))
					old := reflect.New(oldType).Elem()
					for i := 0; i < old.NumField(); i++ {
						old.Field(i).Set(row.FieldByName(oldType.Field(i).Name))
					}
					if err := writer.Write(old.Interface()); err != nil {
						file.Close()
						t.Fatal(err)
					}
				}
				if err := writer.Close(); err != nil {
					file.Close()
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				sum, err := fileSHA256(path)
				if err != nil {
					t.Fatal(err)
				}
				entry := ManifestFile{SchemaVersion: schema, Path: "date=2026-10-03/legacy.parquet", Date: "2026-10-03", SHA256: sum, MinSequence: attempts[0].Sequence, MaxSequence: attempts[len(attempts)-1].Sequence, Records: int64(len(attempts)), Format: FormatParquet}
				for _, a := range attempts {
					row := toParquetAttempt(a)
					entry.InputTokens += row.ProviderInputTokens
					entry.OutputTokens += row.ProviderOutputTokens
					entry.CostMicrosUSD += row.CostMicrosUSD
				}
				if err := e.commitManifest(Manifest{SchemaVersion: schema, LastSequence: f.a.Watermark().Sequence, CompletePrefix: true, Files: []ManifestFile{entry}}); err != nil {
					t.Fatal(err)
				}
				legacy, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits())
				if err != nil || legacy.SourceRows != 2 || legacy.ArchiveRows != 2 {
					t.Fatal("historical columns require no catalog rewrite", legacy, err)
				}
				if _, err := e.MigrateArchiveCatalog(context.Background(), time.Minute); err != nil {
					t.Fatal(err)
				}
				report, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits())
				if err != nil || report.SourceRows != 2 || report.ArchiveRows != 2 {
					t.Fatal(report, err)
				}
			})
		}
	}
}

type verificationTreeEntry struct {
	info   fs.FileInfo
	digest [32]byte
}

func verificationArchiveTree(t *testing.T, root string) map[string]verificationTreeEntry {
	t.Helper()
	result := make(map[string]verificationTreeEntry)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil // Scratch creation may update the root directory mtime.
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[path] = verificationTreeEntry{info, sha256.Sum256(data)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertVerificationArchivePreserved(t *testing.T, before, after map[string]verificationTreeEntry) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatal("verification created, removed or retained archive files", len(before), len(after))
	}
	for path, old := range before {
		now, found := after[path]
		if !found || old.digest != now.digest || !os.SameFile(old.info, now.info) || old.info.Mode() != now.info.Mode() || !old.info.ModTime().Equal(now.info.ModTime()) {
			t.Fatal("verification changed archive file", path)
		}
	}
}

func TestArchiveVerifyLegacyCatalogStreamsInheritedSchemaWithoutMigration(t *testing.T) {
	for _, container := range []string{FormatParquet, FormatNDJSON} {
		t.Run(container, func(t *testing.T) {
			f := diskExportFixture(t, 8)
			e, err := NewExporterWithOptions(t.TempDir(), Options{Format: container})
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := e.Export(f.a.Snapshot())
			if err != nil {
				t.Fatal(err)
			}
			for i := range manifest.Files {
				manifest.Files[i].SchemaVersion = 0
			}
			files, err := json.Marshal(manifest.Files)
			if err != nil {
				t.Fatal(err)
			}
			// Files precede the schema/sequence fields on purpose. The first walk
			// establishes those scalars; the second streams normalized entries.
			raw := []byte(fmt.Sprintf(`{"files":%s,"last_sequence":%d,"schema_version":%d}`, files, manifest.LastSequence, manifest.SchemaVersion))
			if err := os.WriteFile(filepath.Join(e.root, "manifest.json"), raw, 0o400); err != nil {
				t.Fatal(err)
			}
			before := verificationArchiveTree(t, e.root)
			report, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits())
			if err != nil || report.SourceRows != 8 || report.ArchiveRows != 8 || report.Files != len(manifest.Files) || report.ThroughSequence != manifest.LastSequence || report.SourceHead != f.a.Watermark() {
				t.Fatal(report, err)
			}
			assertVerificationArchivePreserved(t, before, verificationArchiveTree(t, e.root))
		})
	}
}

func TestArchiveVerifyLegacyCatalogCannotInferMissingPrefixOrEmptyCoverage(t *testing.T) {
	for _, scenario := range []string{"missing-prefix", "empty", "duplicate-path", "bad-schema", "bad-format", "bad-sha", "after-head", "files-budget", "read-budget"} {
		t.Run(scenario, func(t *testing.T) {
			f := diskExportFixture(t, 8)
			e, err := NewExporterWithOptions(t.TempDir(), Options{Format: FormatNDJSON})
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := e.Export(f.a.Snapshot())
			if err != nil || len(manifest.Files) < 2 {
				t.Fatal(manifest, err)
			}
			limits := DefaultArchiveVerifyLimits()
			after := uint64(0)
			switch scenario {
			case "missing-prefix":
				manifest.Files = manifest.Files[1:]
			case "empty":
				manifest.Files = nil
			case "duplicate-path":
				manifest.Files = append(manifest.Files, manifest.Files[0])
			case "bad-schema":
				manifest.Files[0].SchemaVersion = 100
			case "bad-format":
				manifest.Files[0].Format = "unbounded"
			case "bad-sha":
				manifest.Files[0].SHA256 = "abc"
			case "after-head":
				after = manifest.LastSequence + 1
			case "files-budget":
				limits.Files = 1
			case "read-budget":
				limits.ReadBytes = 1
			}
			if err := e.commitManifest(manifest); err != nil {
				t.Fatal(err)
			}
			before := verificationArchiveTree(t, e.root)
			report, err := e.VerifyDiskArchiveAfter(context.Background(), f.p, after, limits)
			if err == nil || report != (ArchiveVerification{}) {
				t.Fatal("invalid legacy coverage credited", report, err)
			}
			assertVerificationArchivePreserved(t, before, verificationArchiveTree(t, e.root))
		})
	}
}

func TestArchiveVerifyWideNDJSONAndSharedReadBudget(t *testing.T) {
	events := diskFailureRequest("wide_verify", "provider_error", 1, false, false)
	for i := range events {
		if events[i].Kind == ledger.EventAttemptSettled {
			events[i].RequestedModel = strings.Repeat("x", 96<<10)
		}
	}
	f := newDiskFailureFixture(t, events)
	e, err := NewExporterWithOptions(t.TempDir(), Options{Format: FormatNDJSON})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits()); err != nil {
		t.Fatal(err)
	}
	report, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits())
	if err != nil || report.SourceRows != 1 {
		t.Fatal(report, err)
	}
	position, err := e.ReadArchivePosition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	page, err := e.readCatalogPage(context.Background(), position.CatalogHead)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(e.root, page.Files[0].Path))
	if err != nil {
		t.Fatal(err)
	}
	rootInfo, err := os.Stat(filepath.Join(e.root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Enough to copy every byte, but insufficient to decode that private copy.
	limits := DefaultArchiveVerifyLimits()
	limits.ReadBytes = rootInfo.Size() + int64(position.CatalogHead.Bytes) + 1 + info.Size()
	report, err = e.VerifyDiskArchive(context.Background(), f.p, limits)
	if !errors.Is(err, ErrDiskUsageLimit) || report != (ArchiveVerification{}) {
		t.Fatal("decode escaped job IO cap", report, err)
	}
	budget := &archiveVerifyReadBudget{remaining: 4}
	reader := &archiveVerifyReaderAt{ctx: context.Background(), file: strings.NewReader("abcd"), budget: budget}
	if _, err := reader.ReadAt(make([]byte, 4), 0); err != nil {
		t.Fatal(err)
	}
	reader.file = strings.NewReader("efgh")
	if _, err := reader.ReadAt(make([]byte, 1), 0); !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal("new file reset budget", err)
	}
}

type archiveVerifyBlockingReadAt struct {
	entered, release chan struct{}
	once             sync.Once
}

func (r *archiveVerifyBlockingReadAt) ReadAt(b []byte, _ int64) (int, error) {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	return 0, io.EOF
}

func TestArchiveVerifyReadLifetimeJoinsCloseAfterCancellation(t *testing.T) {
	f := diskExportFixture(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocker := &archiveVerifyBlockingReadAt{entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	unblock := func() { release.Do(func() { close(blocker.release) }) }
	t.Cleanup(unblock)
	done := make(chan error, 1)
	go func() {
		_, err := f.p.archiveJob(ctx, DefaultArchiveVerifyLimits().Source, func(ctx context.Context) (DiskArchiveCut, error) {
			reader := &archiveVerifyReaderAt{ctx: ctx, file: blocker, budget: &archiveVerifyReadBudget{remaining: 16}}
			_, err := reader.ReadAt(make([]byte, 8), 0)
			return DiskArchiveCut{}, err
		})
		done <- err
	}()
	select {
	case <-blocker.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("physical read did not start")
	}
	cancel()
	closed := make(chan error, 1)
	go func() { closed <- f.p.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for !f.p.retired.Load() {
		if time.Now().After(deadline) {
			t.Fatal("Close did not withdraw")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-closed:
		t.Fatal("Close passed physical read", err)
	default:
	}
	if f.p.db.Stats().OpenTxN != 0 {
		t.Fatal("physical read holds source transaction")
	}
	unblock()
	if err := <-done; !errors.Is(err, context.Canceled) || !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func TestArchiveVerifyEmptyCatalogCannotHideNativeAttempts(t *testing.T) {
	f := diskExportFixture(t, 2)
	e, err := NewExporter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.commitManifest(Manifest{SchemaVersion: 8, LastSequence: f.a.Watermark().Sequence, CompletePrefix: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.MigrateArchiveCatalog(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if report, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits()); err == nil || report != (ArchiveVerification{}) {
		t.Fatal("empty archive manufactured retention floor", report, err)
	}
	// An explicit caller-selected empty retained interval is different evidence.
	if report, err := e.VerifyDiskArchiveAfter(context.Background(), f.p, f.a.Watermark().Sequence, DefaultArchiveVerifyLimits()); err != nil || report.ArchiveRows != 0 || report.SourceRows != 0 {
		t.Fatal(report, err)
	}
	if report, err := e.VerifyDiskArchiveAfter(context.Background(), f.p, f.a.Watermark().Sequence+1, DefaultArchiveVerifyLimits()); !errors.Is(err, ErrDiskUsageCursor) || report != (ArchiveVerification{}) {
		t.Fatal(report, err)
	}
	empty := newDiskFailureFixture(t, nil)
	genesis, err := NewExporter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := genesis.ExportDisk(context.Background(), empty.p, DefaultDiskExportLimits()); err != nil {
		t.Fatal(err)
	}
	if report, err := genesis.VerifyDiskArchive(context.Background(), empty.p, DefaultArchiveVerifyLimits()); err != nil || report.ArchiveRows != 0 || report.SourceRows != 0 || report.ThroughSequence != 0 {
		t.Fatal(report, err)
	}
}

func TestArchiveVerifyDuplicateRowsAcrossPhysicalPartitions(t *testing.T) {
	f := diskExportFixture(t, 2)
	e, err := NewExporterWithOptions(t.TempDir(), Options{Format: FormatNDJSON})
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(e.root, "date=2026-10-03")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	row := toParquetAttempt(f.a.Snapshot().Attempts[0])
	var entries []ManifestFile
	for _, name := range []string{"a.ndjson", "b.ndjson"} {
		path := filepath.Join(directory, name)
		if err := writeNDJSONAtomic(context.Background(), path, []parquetAttempt{row}, nil); err != nil {
			t.Fatal(err)
		}
		sum, err := fileSHA256(path)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, ManifestFile{SchemaVersion: 8, Path: "date=2026-10-03/" + name, Date: "2026-10-03", SHA256: sum, MinSequence: uint64(row.Sequence), MaxSequence: uint64(row.Sequence), Records: 1, InputTokens: row.ProviderInputTokens, OutputTokens: row.ProviderOutputTokens, CostMicrosUSD: row.CostMicrosUSD, Format: FormatNDJSON})
	}
	if err := e.commitManifest(Manifest{SchemaVersion: 8, LastSequence: f.a.Watermark().Sequence, CompletePrefix: true, Files: entries}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.MigrateArchiveCatalog(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	report, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits())
	if !errors.Is(err, ErrDiskUsageCorrupt) || !strings.Contains(err.Error(), "duplicate archive EventID or sequence") || report != (ArchiveVerification{}) {
		t.Fatal(report, err)
	}
}
