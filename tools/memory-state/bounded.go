package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
)

type boundedProjectionReport struct {
	NativeRecords       uint64 `json:"native_records"`
	NativeBatches       uint64 `json:"native_batches"`
	NativeSyncs         uint64 `json:"native_syncs"`
	NativeMaxBatch      int    `json:"native_max_batch"`
	NativeFileBytes     int64  `json:"native_file_bytes"`
	LedgerFileBytes     int64  `json:"ledger_projection_file_bytes"`
	UsageFileBytes      int64  `json:"usage_projection_file_bytes"`
	LedgerDiskCeiling   int64  `json:"ledger_projection_ceiling_bytes"`
	UsageDiskCeiling    int64  `json:"usage_projection_ceiling_bytes"`
	NativeDiskCeiling   int64  `json:"native_file_safety_ceiling_bytes"`
	NativeAppendNanos   int64  `json:"native_append_nanos"`
	LedgerBuildNanos    int64  `json:"ledger_projection_build_nanos"`
	UsageBuildNanos     int64  `json:"usage_projection_build_nanos"`
	LedgerHeadSequence  uint64 `json:"ledger_head_sequence"`
	UsageHeadSequence   uint64 `json:"usage_head_sequence"`
	UsageRequests       int64  `json:"usage_requests"`
	UsageAttempts       int64  `json:"usage_attempts"`
	UsageCostMicrosUSD  int64  `json:"usage_cost_micros_usd"`
	ConcurrentAppenders int    `json:"concurrent_appenders"`
}

const (
	boundedReplayBatchRows  = 512
	boundedReplayBatchBytes = 8 << 20
)

// boundedProjectionFixture exercises the current Standalone cold-rebuild shape:
// authenticated native frames, a fresh limited Ledger projection, and a fresh
// limited Usage projection rebuilt from that same native prefix. It intentionally
// keeps the open owners alive until the caller has sampled memory.
type boundedProjectionFixture struct {
	root       string
	native     *ledger.Log
	projection *ledger.LedgerProjection
	derivative *usage.LedgerDerivative
	usage      *usage.DiskProjection
	report     *boundedProjectionReport
}

func (f *boundedProjectionFixture) Close() error {
	if f == nil {
		return nil
	}
	var errs []error
	if f.derivative != nil {
		f.derivative.Close()
		f.derivative = nil
	}
	if f.usage != nil {
		errs = append(errs, f.usage.Close())
		f.usage = nil
	}
	if f.projection != nil {
		errs = append(errs, f.projection.Close())
		f.projection = nil
	}
	if f.native != nil {
		errs = append(errs, f.native.Close())
		f.native = nil
	}
	if f.root != "" {
		errs = append(errs, os.RemoveAll(f.root))
		f.root = ""
	}
	return errors.Join(errs...)
}

func buildBoundedProjectionFixture(o options) (fixture *boundedProjectionFixture, resultErr error) {
	fixture = &boundedProjectionFixture{}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, fixture.Close())
		}
	}()
	root, err := os.MkdirTemp(o.directory, "memory-state-bounded-")
	if err != nil {
		return nil, err
	}
	fixture.root = root
	nativePath := filepath.Join(root, "ledger.wal")
	key := bytes.Repeat([]byte{0x4d}, 32)
	native, err := ledger.OpenWithOptions(nativePath, ledger.NewStatus(), ledger.Options{
		ChainKey: key, QueueCapacity: max(1024, o.workers*128), MaxBatch: 64,
	})
	if err != nil {
		return nil, err
	}
	fixture.native = native
	nativeStarted := time.Now()
	if err := appendBoundedFixture(native, nativePath, o); err != nil {
		return nil, err
	}
	nativeAppendNanos := time.Since(nativeStarted).Nanoseconds()
	if err := enforceNativeFixtureCeiling(nativePath, o.nativeMaxBytes); err != nil {
		return nil, err
	}
	head, _, ok := native.ChainHead()
	if !ok || head.Sequence != uint64(o.requests*5) {
		return nil, fmt.Errorf("native fixture head mismatch: %v ok=%t", head, ok)
	}
	projectionPath := filepath.Join(root, "ledger-projection.db")
	ledgerBuildStarted := time.Now()
	projection, prefix, err := native.BuildProjectionFromCursorWithLimit(
		context.Background(), head.Generation, head.Sequence, projectionPath, o.ledgerMaxBytes,
	)
	if err != nil {
		return nil, fmt.Errorf("build authenticated Ledger projection: %w", err)
	}
	ledgerBuildNanos := time.Since(ledgerBuildStarted).Nanoseconds()
	fixture.projection = projection
	if prefix.Head != head {
		return nil, fmt.Errorf("Ledger projection cut %v differs from native head %v", prefix.Head, head)
	}
	usagePath := filepath.Join(root, "usage-projection.db")
	derivative, disk, err := usage.NewDiskLedgerDerivativeWithLimit(projection, usagePath, o.usageMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("create bounded Usage projection: %w", err)
	}
	fixture.derivative, fixture.usage = derivative, disk
	usageBuildStarted := time.Now()
	batch := make([]ledger.Record, 0, boundedReplayBatchRows)
	batchBytes := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := derivative.ApplyReplayBatchContext(context.Background(), batch); err != nil {
			return err
		}
		clear(batch)
		batch = batch[:0]
		batchBytes = 0
		return nil
	}
	usageHead, err := native.ReplayAuthenticated(context.Background(), ledger.ReplayAnchor{}, func(record ledger.Record) error {
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if len(raw) > boundedReplayBatchBytes {
			return fmt.Errorf("Usage replay record exceeds 8 MiB batch budget")
		}
		if batchBytes+len(raw) > boundedReplayBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, record)
		batchBytes += len(raw)
		if len(batch) == boundedReplayBatchRows {
			return flush()
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("rebuild bounded Usage projection from authenticated native history: %w", err)
	}
	if err := flush(); err != nil {
		return nil, fmt.Errorf("apply final bounded Usage replay page: %w", err)
	}
	usageBuildNanos := time.Since(usageBuildStarted).Nanoseconds()
	ledgerHead, _, err := projection.ReadPosition(context.Background())
	if err != nil {
		return nil, fmt.Errorf("read rebuilt Ledger position: %w", err)
	}
	usageStats, err := disk.ReadStats(context.Background())
	if err != nil {
		return nil, fmt.Errorf("read rebuilt Usage statistics: %w", err)
	}
	expectedCost := int64(o.requests * 80)
	if o.fixture == "priced" {
		expectedCost = int64(o.requests * 13)
	}
	if ledgerHead != head || usageHead != head || usageStats.Watermark != head ||
		usageStats.Totals.Requests != int64(o.requests) || usageStats.Totals.Attempts != int64(o.requests) ||
		usageStats.Totals.CostMicrosUSD != expectedCost || usageStats.Metrics.ActiveRequests != 0 {
		return nil, fmt.Errorf("bounded projection fixture mismatch: ledger=%v usage=%v stats=%+v expected_cost=%d", ledgerHead, usageHead, usageStats, expectedCost)
	}
	nativeInfo, err := os.Stat(nativePath)
	if err != nil {
		return nil, err
	}
	ledgerInfo, err := os.Stat(projectionPath)
	if err != nil {
		return nil, err
	}
	usageInfo, err := os.Stat(usagePath)
	if err != nil {
		return nil, err
	}
	stats := native.Stats()
	fixture.report = &boundedProjectionReport{
		NativeRecords: uint64(o.requests * 5), NativeBatches: stats.Batches, NativeSyncs: stats.Syncs,
		NativeMaxBatch: stats.MaxBatch, NativeFileBytes: nativeInfo.Size(), LedgerFileBytes: ledgerInfo.Size(),
		UsageFileBytes: usageInfo.Size(), LedgerDiskCeiling: o.ledgerMaxBytes, UsageDiskCeiling: o.usageMaxBytes,
		NativeDiskCeiling: o.nativeMaxBytes, NativeAppendNanos: nativeAppendNanos,
		LedgerBuildNanos: ledgerBuildNanos, UsageBuildNanos: usageBuildNanos,
		LedgerHeadSequence: ledgerHead.Sequence, UsageHeadSequence: usageStats.Watermark.Sequence,
		UsageRequests: usageStats.Totals.Requests, UsageAttempts: usageStats.Totals.Attempts,
		UsageCostMicrosUSD: usageStats.Totals.CostMicrosUSD, ConcurrentAppenders: min(o.workers, o.requests),
	}
	runtime.KeepAlive(fixture.native)
	runtime.KeepAlive(fixture.projection)
	runtime.KeepAlive(fixture.derivative)
	runtime.KeepAlive(fixture.usage)
	return fixture, nil
}

func appendBoundedFixture(native *ledger.Log, nativePath string, o options) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workers := min(o.workers, o.requests)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	fail := func(err error) {
		once.Do(func() {
			firstErr = err
			cancel()
		})
	}
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(first int) {
			defer wg.Done()
			for request := first; request < o.requests; request += workers {
				if err := ctx.Err(); err != nil {
					return
				}
				records, err := requestRecords(request, o.fixture == "priced")
				if err != nil {
					fail(err)
					return
				}
				for _, record := range records {
					if _, err := native.Append(ctx, record.Event); err != nil {
						fail(fmt.Errorf("append request %d kind %d: %w", request, record.Event.Kind, err))
						return
					}
				}
				// Poll per request to bound the temporary source file without
				// placing a filesystem stat on every generated native frame.
				if request%64 == 0 {
					if err := enforceNativeFixtureCeiling(nativePath, o.nativeMaxBytes); err != nil {
						fail(err)
						return
					}
				}
			}
		}(worker)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return enforceNativeFixtureCeiling(nativePath, o.nativeMaxBytes)
}

func enforceNativeFixtureCeiling(path string, ceiling int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() > ceiling {
		return fmt.Errorf("synthetic native WAL safety ceiling exceeded: %d > %d bytes", info.Size(), ceiling)
	}
	return nil
}
