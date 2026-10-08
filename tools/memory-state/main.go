// memory-state measures synthetic state retention and storage-layout costs.
// It never opens Halro configuration, production directories, or providers.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"time"
	"unsafe"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
)

var sourceCommit = "unknown" // Set with -ldflags '-X main.sourceCommit=...'.

type options struct {
	mode, fixture, directory, profile, cgroup     string
	requests, batch, metadataWrites, payloadBytes int
	workers                                       int
	nativeMaxBytes, ledgerMaxBytes, usageMaxBytes int64
}

type report struct {
	Version           int                      `json:"version"`
	Scope             string                   `json:"scope"`
	Mode              string                   `json:"mode"`
	Fixture           string                   `json:"fixture"`
	SourceCommit      string                   `json:"source_commit"`
	SourceModified    string                   `json:"source_modified"`
	GoVersion         string                   `json:"go_version"`
	OS                string                   `json:"os"`
	Arch              string                   `json:"arch"`
	CPUs              int                      `json:"cpus"`
	GOMAXPROCS        int                      `json:"gomaxprocs"`
	GOGC              string                   `json:"gogc"`
	GOMEMLIMIT        string                   `json:"gomemlimit"`
	Requests          int                      `json:"requests"`
	Events            int                      `json:"events"`
	SampleIntervalMS  int                      `json:"sample_interval_ms"`
	ElapsedNanos      int64                    `json:"elapsed_nanos"`
	Baseline          memoryPoint              `json:"baseline_after_gc"`
	Final             memoryPoint              `json:"final_after_gc"`
	FinalPhase        string                   `json:"final_measurement_phase"`
	Peak              memoryPoint              `json:"sampled_peak"`
	LiveHeapDelta     int64                    `json:"live_heap_delta_bytes"`
	SnapshotHeapDelta *int64                   `json:"snapshot_live_heap_delta_bytes,omitempty"`
	StructBytes       map[string]uintptr       `json:"struct_bytes"`
	Layout            *layoutReport            `json:"layout,omitempty"`
	Bounded           *boundedProjectionReport `json:"bounded_projection,omitempty"`
}

func main() {
	var o options
	flag.StringVar(&o.mode, "mode", "ledger-usage", "ledger, ledger-usage, usage-snapshot, bounded-projections, layout-same, layout-separate")
	flag.StringVar(&o.fixture, "fixture", "priced", "priced or legacy (state modes)")
	flag.IntVar(&o.requests, "requests", 10000, "synthetic request count (five events each)")
	flag.IntVar(&o.batch, "batch", 100, "events per projection transaction (layout modes)")
	flag.IntVar(&o.metadataWrites, "metadata-writes", 1000, "concurrent metadata transactions (layout modes)")
	flag.IntVar(&o.payloadBytes, "payload-bytes", 1024, "fixed history value bytes (layout modes)")
	flag.StringVar(&o.directory, "dir", "", "existing parent for a newly owned temporary layout directory")
	flag.IntVar(&o.workers, "workers", 4, "concurrent synthetic native Ledger append workers (bounded-projections mode)")
	flag.Int64Var(&o.nativeMaxBytes, "native-max-bytes", 0, "required temporary native WAL byte ceiling (bounded-projections mode)")
	flag.Int64Var(&o.ledgerMaxBytes, "ledger-max-bytes", 0, "required fresh Ledger projection byte ceiling (bounded-projections mode)")
	flag.Int64Var(&o.usageMaxBytes, "usage-max-bytes", 0, "required fresh Usage projection byte ceiling (bounded-projections mode)")
	flag.StringVar(&o.profile, "heap-profile", "", "state modes only: write a new heap profile at the final live state; refuses overwrite")
	flag.StringVar(&o.cgroup, "cgroup-dir", "", "explicit cgroup v2 directory to sample; omit if unavailable")
	flag.Parse()
	if err := validateOptions(o); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	r, err := run(o)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(r); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func validateOptions(o options) error {
	if o.requests < 1 || o.requests > 10_000_000 || o.batch < 1 || o.batch > 10000 ||
		o.metadataWrites < 1 || o.payloadBytes < 8 || o.payloadBytes > 1<<20 || o.workers < 1 || o.workers > 64 {
		return fmt.Errorf("invalid workload size; requests=1..10000000, batch=1..10000, metadata-writes>=1, payload-bytes=8..1048576")
	}
	if o.fixture != "legacy" && o.fixture != "priced" {
		return fmt.Errorf("unknown fixture %q", o.fixture)
	}
	if o.cgroup != "" && sampleMemory(o.cgroup).Cgroup == nil {
		return fmt.Errorf("cannot read cgroup v2 memory.current at %q", o.cgroup)
	}
	switch o.mode {
	case "ledger", "ledger-usage", "usage-snapshot", "bounded-projections", "layout-same", "layout-separate":
	default:
		return fmt.Errorf("unknown mode %q", o.mode)
	}
	if o.mode == "bounded-projections" && (o.directory == "" || o.nativeMaxBytes <= 0 || o.ledgerMaxBytes <= 0 || o.usageMaxBytes <= 0) {
		return fmt.Errorf("bounded-projections requires -dir and positive -native-max-bytes, -ledger-max-bytes, and -usage-max-bytes")
	}
	if o.profile != "" && (o.mode == "layout-same" || o.mode == "layout-separate") {
		return fmt.Errorf("layout heap profiles are unsupported; use layout.open_databases_after_scan_gc for the live database measurement")
	}
	return nil
}

func run(o options) (report, error) {
	r := report{Version: 1, Scope: "synthetic local experiment; not production or capacity acceptance",
		Mode: o.mode, Fixture: fixtureVersion + "/" + o.fixture, SourceCommit: sourceCommit,
		GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(),
		GOMAXPROCS: runtime.GOMAXPROCS(0), GOGC: os.Getenv("GOGC"), GOMEMLIMIT: os.Getenv("GOMEMLIMIT"),
		Requests: o.requests, Events: o.requests * 5, SampleIntervalMS: 20, FinalPhase: "state retained",
		StructBytes: map[string]uintptr{"event": unsafe.Sizeof(ledger.Event{}), "record": unsafe.Sizeof(ledger.Record{}),
			"settled_attempt": unsafe.Sizeof(ledger.SettledAttempt{}), "attempt_event": unsafe.Sizeof(usage.AttemptEvent{}),
			"request_summary": unsafe.Sizeof(usage.RequestSummary{})}}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && r.SourceCommit == "unknown" {
				r.SourceCommit = setting.Value
			}
			if setting.Key == "vcs.modified" {
				r.SourceModified = setting.Value
			}
		}
	}
	runtime.GC()
	r.Baseline = sampleMemory(o.cgroup)
	monitor := startMonitor(o.cgroup)
	defer monitor.stop()
	started := time.Now()
	var state *ledger.State
	var aggregate *usage.Aggregate
	var snapshot usage.Snapshot
	var bounded *boundedProjectionFixture
	if o.mode == "bounded-projections" {
		var err error
		bounded, err = buildBoundedProjectionFixture(o)
		if err != nil {
			return r, err
		}
		r.Bounded = bounded.report
		r.FinalPhase = "authenticated native WAL and fresh Ledger/Usage disk projections open"
	} else if o.mode == "layout-same" || o.mode == "layout-separate" {
		layout, err := runLayout(o)
		if err != nil {
			return r, err
		}
		r.Layout, r.Fixture = &layout, "storage-layout-v1/fixed-values"
		r.FinalPhase = "databases closed; use layout.open_databases_after_scan_gc for live databases"
	} else {
		state = ledger.NewState()
		if o.mode != "ledger" {
			aggregate = usage.NewAggregate()
		}
		if err := visitFixture(o.requests, o.fixture == "priced", func(record ledger.Record) error {
			if err := state.Apply(record); err != nil {
				return err
			}
			if aggregate != nil {
				return aggregate.Apply(record)
			}
			return nil
		}); err != nil {
			return r, err
		}
	}
	r.ElapsedNanos = time.Since(started).Nanoseconds()
	runtime.GC()
	r.Final = sampleMemory(o.cgroup)
	r.LiveHeapDelta = int64(r.Final.HeapAlloc) - int64(r.Baseline.HeapAlloc)
	if o.mode == "usage-snapshot" {
		snapshot = aggregate.Snapshot()
		runtime.GC()
		after := sampleMemory(o.cgroup)
		delta := int64(after.HeapAlloc) - int64(r.Final.HeapAlloc)
		r.SnapshotHeapDelta = &delta
		monitor.observe(after)
	}
	monitor.observe(r.Final)
	r.Peak = monitor.stop()
	if o.profile != "" {
		file, err := os.OpenFile(o.profile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return r, err
		}
		writeErr := pprof.WriteHeapProfile(file)
		closeErr := file.Close()
		if writeErr != nil {
			return r, writeErr
		}
		if closeErr != nil {
			return r, closeErr
		}
	}
	runtime.KeepAlive(state)
	runtime.KeepAlive(aggregate)
	runtime.KeepAlive(snapshot)
	if bounded != nil {
		if err := bounded.Close(); err != nil {
			return r, fmt.Errorf("close bounded projection fixture: %w", err)
		}
	}
	return r, nil
}
