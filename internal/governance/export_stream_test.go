package governance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

func streamingExportFixture() ExportInput {
	now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	return ExportInput{
		WorkUnits:           []domain.WorkUnit{{ID: "wku_unit", ProjectID: "prj_1", Status: domain.WorkUnitOpen, CreatedAt: now, OutcomeDefinitions: []domain.OutcomeDefinitionRef{{ID: "odef_result", Version: 1}}}},
		Runs:                []domain.Run{{ID: "run_one", ProjectID: "prj_1", WorkUnitID: "wku_unit", BudgetMicrosUSD: 100, CommittedMicrosUSD: 42, ReservedMicrosUSD: 9, Status: domain.RunActive, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}},
		Outcomes:            []domain.Outcome{{ID: "out_one", ProjectID: "prj_1", WorkUnitID: "wku_unit", DefinitionID: "odef_result", DefinitionVersion: 1, Value: "accepted", ReporterKeyID: strings.Repeat("k", 70<<10), ObservedAt: now, IngestedAt: now, Revision: 1, GovernanceSequence: 9, Provisional: true}},
		Definitions:         []domain.OutcomeDefinition{{ID: "odef_result", ProjectID: "prj_1", Name: "result", Version: 1, DataType: domain.OutcomeCategorical, AllowedValues: []string{"accepted", "rejected"}, SuccessValues: []string{"accepted"}, CreatedAt: now, CreatedBy: "admin"}},
		AccountingWatermark: ledger.Watermark{Generation: 2, Sequence: 123, Offset: 456}, GovernanceSequence: 9, GovernanceOffset: 987,
	}
}

func TestStreamingExportMatchesCompatibilityRowsAndKeepsWideRows(t *testing.T) {
	input := streamingExportFixture()
	first, second := filepath.Join(t.TempDir(), "slice"), filepath.Join(t.TempDir(), "stream")
	a, err := WriteExport(context.Background(), first, input)
	if err != nil {
		t.Fatal(err)
	}
	b, err := WriteExportStream(context.Background(), second, ExportStreamInput{WorkUnits: exportSliceProducer(input.WorkUnits), Runs: exportSliceProducer(input.Runs), Outcomes: exportSliceProducer(input.Outcomes), Definitions: exportSliceProducer(input.Definitions), AccountingWatermark: input.AccountingWatermark, GovernanceSequence: input.GovernanceSequence, GovernanceOffset: input.GovernanceOffset})
	if err != nil || !reflect.DeepEqual(a.Files, b.Files) || a.AccountingWatermark != b.AccountingWatermark || !reflect.DeepEqual(a.GovernanceWatermark, b.GovernanceWatermark) {
		t.Fatal(a, b, err)
	}
	for _, meta := range a.Files {
		left, err := os.ReadFile(filepath.Join(first, meta.Path))
		if err != nil {
			t.Fatal(err)
		}
		right, err := os.ReadFile(filepath.Join(second, meta.Path))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(left, right) || meta.RecordCount != 1 {
			t.Fatal(meta)
		}
		if meta.Dataset == "work_units" || meta.Dataset == "runs" {
			if bytes.Contains(left, []byte("committed_micros_usd")) || bytes.Contains(left, []byte("reserved_micros_usd")) {
				t.Fatal("duplicated additive accounting values")
			}
		}
	}
	verified, err := VerifyExport(second)
	if err != nil || !reflect.DeepEqual(verified.Files, b.Files) {
		t.Fatal(verified, err)
	}
	if string(verified.GovernanceWatermark["sequence"].(json.Number)) != "9" || verified.AccountingWatermark.Sequence != 123 {
		t.Fatal(verified)
	}
}

func TestStreamingExportFailuresClearOwnedOutputAndNeverPublishPrefix(t *testing.T) {
	for _, kind := range []string{"producer", "ignored_sink", "escaped_row", "canceled", "rename", "future_fact", "domain", "encode", "late_dataset"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dir := filepath.Join(t.TempDir(), "export")
			failure := errors.New("failed source after useful prefix")
			var late func(domain.WorkUnit) error
			input := ExportStreamInput{WorkUnits: func(ctx context.Context, yield func(domain.WorkUnit) error) error {
				late = yield
				if err := yield(domain.WorkUnit{ID: "wku_first", ProjectID: "prj_1"}); err != nil {
					return err
				}
				switch kind {
				case "producer":
					return failure
				case "ignored_sink":
					_ = yield(domain.WorkUnit{ProjectID: strings.Repeat("x", ExportRowMaxBytes+1)})
				case "escaped_row":
					_ = yield(domain.WorkUnit{ProjectID: strings.Repeat("\x00", ExportRowMaxBytes/2)})
				case "canceled":
					cancel()
				case "rename":
					return os.Mkdir(filepath.Join(dir, "work_units.ndjson"), 0700)
				case "domain":
					_ = yield(domain.WorkUnit{OutcomeDefinitions: make([]domain.OutcomeDefinitionRef, 9)})
				case "encode":
					_ = yield(domain.WorkUnit{CreatedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)})
				}
				return nil
			}}
			if kind == "future_fact" {
				input.Outcomes = func(ctx context.Context, yield func(domain.Outcome) error) error {
					return yield(domain.Outcome{GovernanceSequence: 1})
				}
			}
			if kind == "late_dataset" {
				input.Definitions = func(context.Context, func(domain.OutcomeDefinition) error) error { return failure }
			}
			result, err := WriteExportStream(ctx, dir, input)
			if err == nil || !reflect.DeepEqual(result, ExportManifest{}) {
				t.Fatal(result, err)
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed call left output", err)
			}
			if late != nil && !errors.Is(late(domain.WorkUnit{}), ErrExportStreamClosed) {
				t.Fatal("late callback remained writable")
			}
		})
	}
}

func TestStreamingExportDoesNotOverwriteExistingOrEmptyDirectory(t *testing.T) {
	for _, empty := range []bool{false, true} {
		dir := filepath.Join(t.TempDir(), "export")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if !empty {
			if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("previous export"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		result, err := WriteExport(context.Background(), dir, ExportInput{})
		if !errors.Is(err, os.ErrExist) || !reflect.DeepEqual(result, ExportManifest{}) {
			t.Fatal(result, err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if empty && len(entries) != 0 {
			t.Fatal(entries)
		}
		if !empty {
			raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
			if err != nil || string(raw) != "previous export" {
				t.Fatal(string(raw), err)
			}
		}
	}
}

func TestStreamingExportSerializesBoundedConcurrentCallbacksAndSealsRetainedSink(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "export")
	var late func(domain.WorkUnit) error
	input := ExportStreamInput{WorkUnits: func(ctx context.Context, yield func(domain.WorkUnit) error) error {
		late = yield
		var wg sync.WaitGroup
		failures := make(chan error, 8)
		for worker := 0; worker < 8; worker++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				for i := 0; i < 20; i++ {
					if err := yield(domain.WorkUnit{ID: fmt.Sprintf("wku_%d_%d", worker, i), ProjectID: "prj_1"}); err != nil {
						failures <- err
						return
					}
				}
			}(worker)
		}
		wg.Wait()
		close(failures)
		for err := range failures {
			return err
		}
		return nil
	}}
	manifest, err := WriteExportStream(context.Background(), dir, input)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(late(domain.WorkUnit{}), ErrExportStreamClosed) {
		t.Fatal("retained sink accepted a late row")
	}
	verified, err := VerifyExport(dir)
	if err != nil || !reflect.DeepEqual(verified.Files, manifest.Files) {
		t.Fatal(verified, err)
	}
	for _, file := range verified.Files {
		if file.Dataset == "work_units" && file.RecordCount != 160 {
			t.Fatal(file)
		}
	}
}

func TestStreamingExportVerificationRejectsManifestAndDatasetTampering(t *testing.T) {
	for _, kind := range []string{"count", "min_sequence", "max_sequence", "future_sequence", "zero_sequence", "checksum", "missing", "non_object", "wide_line", "manifest_limit", "trailing_manifest", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "export")
			manifest, err := WriteExport(context.Background(), dir, streamingExportFixture())
			if err != nil {
				t.Fatal(err)
			}
			var target *ExportFile
			for i := range manifest.Files {
				if manifest.Files[i].Dataset == "outcomes" {
					target = &manifest.Files[i]
				}
			}
			path := filepath.Join(dir, target.Path)
			switch kind {
			case "count":
				target.RecordCount++
			case "min_sequence":
				target.MinSequence = 1
			case "max_sequence":
				target.MaxSequence = 8
			case "future_sequence":
				manifest.GovernanceWatermark["sequence"] = 8
			case "zero_sequence", "non_object", "wide_line", "checksum":
				body := []byte("{\"governance_sequence\":0}\n")
				if kind == "non_object" {
					body = []byte("[]\n")
				}
				if kind == "wide_line" {
					body = []byte("{\"wide\":\"" + strings.Repeat("x", ExportRowMaxBytes) + "\"}\n")
				}
				if err := os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
				if kind != "checksum" {
					target.SHA256 = fmt.Sprintf("sha256:%x", sha256.Sum256(body))
				}
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				other := filepath.Join(t.TempDir(), "data")
				if err := os.Rename(path, other); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, path); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "manifest_limit" {
				raw = append(raw, bytes.Repeat([]byte(" "), ExportManifestMaxBytes)...)
			}
			if kind == "trailing_manifest" {
				raw = append(raw, []byte(" {}")...)
			}
			if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			result, err := VerifyExport(dir)
			if err == nil || !reflect.DeepEqual(result, ExportManifest{}) {
				t.Fatal("bad export verified", result, err)
			}
		})
	}
}

func TestStreamingExportEmptyAndCanceledVerification(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "empty")
	manifest, err := WriteExportStream(context.Background(), dir, ExportStreamInput{})
	if err != nil || len(manifest.Files) != 4 {
		t.Fatal(manifest, err)
	}
	if _, err := VerifyExport(dir); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result, err := VerifyExportContext(ctx, dir); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(result, ExportManifest{}) {
		t.Fatal(result, err)
	}
	if result, err := WriteExportStream(ctx, filepath.Join(t.TempDir(), "canceled"), ExportStreamInput{}); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(result, ExportManifest{}) {
		t.Fatal(result, err)
	}
}

func TestStreamingExportFirstParentCreationSyncsEveryNewDirectoryEntry(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "governance")
	second := filepath.Join(first, "export")
	var barriers []string
	err := ensureExportParent(context.Background(), second, func(path string) error { barriers = append(barriers, path); return syncExportDirectory(path) })
	if err != nil || (len(barriers) < 4 || !reflect.DeepEqual(barriers[:3], []string{second, first, root}) || barriers[len(barriers)-1] != filepath.VolumeName(root)+string(filepath.Separator)) {
		t.Fatal(barriers, err)
	}
	if _, err := WriteExportStream(context.Background(), filepath.Join(second, "first"), ExportStreamInput{}); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyExport(filepath.Join(second, "first")); err != nil {
		t.Fatal(err)
	}
	for stop := 0; stop < 4; stop++ {
		root := t.TempDir()
		first := filepath.Join(root, "governance")
		second := filepath.Join(first, "export")
		calls := 0
		failure := errors.New("directory barrier failed")
		err := ensureExportParent(context.Background(), second, func(path string) error {
			current := calls
			calls++
			if current == stop {
				return failure
			}
			return syncExportDirectory(path)
		})
		if !errors.Is(err, failure) || calls != stop+1 {
			t.Fatal(stop, calls, err)
		}
		if _, err := os.Stat(filepath.Join(second, "first", "manifest.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("parent failure published an export", err)
		}
		// Failed barriers can leave existing parents. Retry must resync them,
		// not assume an existing directory is durable merely because it exists.
		var retry []string
		if err := ensureExportParent(context.Background(), second, func(path string) error { retry = append(retry, path); return syncExportDirectory(path) }); err != nil {
			t.Fatal(err)
		}
		if len(retry) < 3 || !reflect.DeepEqual(retry[:3], []string{second, first, root}) {
			t.Fatal(retry)
		}

	}
}

// Real file IO and periodic forced GC sample retained Go heap during generation.
// It excludes Runtime/owner fixtures, mmap, RSS/cgroup and production budgets.
func BenchmarkStreamingExportHistory(b *testing.B) {
	for _, count := range []int{128, 8192} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			parent := b.TempDir()
			reporter := strings.Repeat("k", 1024)
			now := time.Now().UTC()
			var observedPeak uint64
			for iteration := 0; iteration < b.N; iteration++ {
				runtime.GC()
				var start runtime.MemStats
				runtime.ReadMemStats(&start)
				dir := filepath.Join(parent, fmt.Sprint(iteration))
				input := ExportStreamInput{GovernanceSequence: uint64(count), Outcomes: func(ctx context.Context, yield func(domain.Outcome) error) error {
					for i := 0; i < count; i++ {
						row := domain.Outcome{ID: fmt.Sprintf("out_%d", i), ProjectID: "prj_1", WorkUnitID: fmt.Sprintf("wku_%d", i), DefinitionID: "odef_result", DefinitionVersion: 1, Value: "accepted", ReporterKeyID: reporter, ObservedAt: now, IngestedAt: now, Revision: 1, GovernanceSequence: uint64(i + 1)}
						if err := yield(row); err != nil {
							return err
						}
						if i%128 == 127 {
							runtime.GC()
							var sample runtime.MemStats
							runtime.ReadMemStats(&sample)
							if sample.HeapAlloc > start.HeapAlloc && sample.HeapAlloc-start.HeapAlloc > observedPeak {
								observedPeak = sample.HeapAlloc - start.HeapAlloc
							}
						}
					}
					return nil
				}}
				if _, err := WriteExportStream(context.Background(), dir, input); err != nil {
					b.Fatal(err)
				}
				if _, err := VerifyExport(dir); err != nil {
					b.Fatal(err)
				}
				if err := os.RemoveAll(dir); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(observedPeak), "sampled_live_heap_extra_B")
		})
	}
}
