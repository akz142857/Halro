package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestPackedTypedUsageTenThousandInterleavedRequestsFixedCeiling(t *testing.T) {
	if os.Getenv("HALRO_PACKED_USAGE_CAPACITY") != "1" {
		t.Skip("explicit 10k/50k capacity acceptance; set HALRO_PACKED_USAGE_CAPACITY=1")
	}
	ctx := context.Background()
	dir := t.TempDir()
	key := bytes.Repeat([]byte{0x67}, 32)
	path := filepath.Join(dir, "ledger.wal")
	fixture := os.Getenv("HALRO_PACKED_USAGE_NATIVE_FIXTURE")
	if fixture != "" {
		data, err := os.ReadFile(fixture)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	native, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key, MaxBatch: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	kinds := [...]ledger.EventKind{ledger.EventRequestAccepted, ledger.EventReservationCreated, ledger.EventAttemptStarted, ledger.EventAttemptSettled, ledger.EventRequestFinalized}
	for first := 0; fixture == "" && first < 10000; first += 64 {
		results := make(chan error, 64)
		for i := first; i < min(first+64, 10000); i++ {
			go func(i int) {
				for step, kind := range kinds {
					base := start.Add(time.Duration(i) * 10 * time.Millisecond)
					e := ledger.Event{EventID: fmt.Sprintf("capacity_%09d_%d", i, step), Kind: kind, RequestID: fmt.Sprintf("capacity_req_%09d", i), AttemptID: fmt.Sprintf("capacity_att_%09d", i), ProjectID: "prj_capacity", KeyID: "key_capacity", RouteID: "route_capacity", DeploymentID: "dep_capacity", ProviderID: "provider_capacity", RequestedModel: "synthetic", ProviderModel: "synthetic", PeriodID: base.Format("2006-01-02"), PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, PeriodStartMicros: start.Truncate(24 * time.Hour).UnixMicro(), PeriodEndMicros: start.Truncate(24 * time.Hour).Add(24 * time.Hour).UnixMicro(), OccurredAt: base.Add(time.Duration(step) * time.Millisecond)}
					switch kind {
					case ledger.EventRequestAccepted, ledger.EventRequestFinalized:
						e.AttemptID = ""
					case ledger.EventReservationCreated:
						e.ReservationMicrosUSD = ledger.MicrosUSD(100)
					case ledger.EventAttemptSettled:
						e.CommittedMicrosUSD = ledger.MicrosUSD(80)
						e.ProviderInputTokens, e.ProviderOutputTokens = 7, 3
						e.TokenUsageSource = ledger.TokenUsageSourceProvider
						e.AttemptNumber, e.LatencyMillis = 1, 3
					}
					if kind == ledger.EventAttemptSettled || kind == ledger.EventRequestFinalized {
						e.Outcome = "success"
					}
					if _, err := native.AppendWithReceipt(ctx, e); err != nil {
						results <- fmt.Errorf("request %d step %d: %w", i, step, err)
						return
					}
				}
				results <- nil
			}(i)
		}
		var joined error
		for i := first; i < min(first+64, 10000); i++ {
			joined = errors.Join(joined, <-results)
		}
		if joined != nil {
			t.Fatal(joined)
		}
	}
	t.Log("native append complete")
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 1 << 30, Disk: 1 << 30}, MaxClaims: 64, MaxOwners: 8, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 64 * resourcebudget.ClaimRAMBytes}
	manager, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	report, partial, err := ledger.InspectReplayAuthenticated(path, key, nil)
	if err != nil || partial {
		t.Fatal(report, partial, err)
	}
	owner, err := ledger.BuildPackedAuthenticatedLedgerProjection(ctx, path, key, filepath.Join(dir, "ledger-root"), report, ledger.AuthenticatedLedgerOptions{Identity: sha256.Sum256([]byte("capacity-ledger")), MaxDiskBytes: 128 << 20, MaxAppendBytes: 16 << 20, Budget: manager})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Discard()
	lease, err := native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	before := manager.Snapshot().Used
	options := AuthenticatedUsageOptions{Native: native, Owner: owner, Identity: sha256.Sum256([]byte("capacity-usage")), UsageKey: key, ScratchPath: filepath.Join(dir, "usage.reducer.db"), ScratchMaxDiskBytes: 256 << 20, RootDirectory: filepath.Join(dir, "usage-root"), RootMaxDiskBytes: 256 << 20, RootMaxAppendBytes: 16 << 20, Budget: manager}
	var scratchBytes int64
	packed, ref, err := buildPackedTypedUsageProjection(ctx, options, lease, func(phase string) {
		if phase == "before-publication" {
			info, err := os.Stat(options.ScratchPath)
			if err != nil {
				t.Fatal(err)
			}
			scratchBytes = info.Size()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer packed.Discard()
	stats, err := packed.ReadStats(ctx)
	if err != nil || stats.Totals.Attempts != 10000 || stats.Metrics.ActiveRequests != 0 || ref.Ledger.Anchor.Head.Sequence != 50000 {
		t.Fatal(stats, ref, err)
	}
	var ordinary *DiskProjection
	err = owner.WithOfflineMaintenanceUsageSource(ctx, native, lease, manager, ref.Ledger.Scope, func(ctx context.Context, source ledger.AuthenticatedUsageSource) error {
		var err error
		ordinary, err = CreateDiskProjection(filepath.Join(dir, "ordinary.db"), packedUsageSource{source})
		if err != nil {
			return err
		}
		batch := make([]ledger.Record, 0, diskReplayBatchRows)
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			err := ordinary.ApplyReplayBatchContext(ctx, batch)
			clear(batch)
			batch = batch[:0]
			return err
		}
		_, err = source.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, func(record ledger.Record) error {
			batch = append(batch, record)
			if len(batch) == cap(batch) {
				return flush()
			}
			return nil
		})
		if err != nil {
			return err
		}
		return flush()
	})
	if ordinary != nil {
		defer ordinary.Discard()
	}
	if err != nil {
		t.Fatal(err)
	}
	compareTypedUsageRows(t, packed, ordinary)
	identity, err := packed.engine.cow.RootIdentity(ctx)
	if err != nil || identity.Revision != 1 || scratchBytes >= 256<<20 {
		t.Fatal(identity, scratchBytes, err)
	}
	t.Logf("requests=10000 facts=50000 scratch_bytes=%d root_bytes=%d rows=%d revision=%d; all namespaces match ordinary 512-record replay", scratchBytes, ref.Projection.Root.State.Extent, identity.Rows, identity.Revision)
	if err := packed.Discard(); err != nil {
		t.Fatal(err)
	}
	if after := manager.Snapshot().Used; after != before {
		t.Fatal("construction fees remain", before, after)
	}
}
