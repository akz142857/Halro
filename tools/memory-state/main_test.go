package main

import (
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
)

func TestFixtureCompletesAccountingAndUsage(t *testing.T) {
	for _, priced := range []bool{false, true} {
		state, aggregate := ledger.NewState(), usage.NewAggregate()
		if err := visitFixture(3, priced, func(record ledger.Record) error {
			if err := record.Event.Validate(); err != nil {
				return err
			}
			if err := state.Apply(record); err != nil {
				return err
			}
			return aggregate.Apply(record)
		}); err != nil {
			t.Fatal(err)
		}
		want := int64(240)
		if priced {
			want = 39
		}
		balance := state.Balance("project_1", "2026-10-02", 1)
		if balance.CommittedMicrosUSD != want || balance.ReservedMicrosUSD != 0 {
			t.Fatalf("balance=%+v priced=%t", balance, priced)
		}
		if aggregate.Metrics().ActiveRequests != 0 || aggregate.Watermark() != state.Watermark() {
			t.Fatalf("incomplete lifecycle priced=%t", priced)
		}
		snapshot := aggregate.Snapshot()
		if snapshot.Totals.CostMicrosUSD != want || len(snapshot.Attempts) != 3 || len(snapshot.Requests) != 3 {
			t.Fatalf("usage mismatch priced=%t", priced)
		}
	}
}

func TestBoundedProjectionFixtureRebuildsAuthenticatedDiskOwners(t *testing.T) {
	r, err := run(options{
		mode: "bounded-projections", fixture: "priced", requests: 3, workers: 2,
		directory: t.TempDir(), nativeMaxBytes: 1 << 20,
		ledgerMaxBytes: 32 << 20, usageMaxBytes: 32 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Bounded == nil || r.Bounded.NativeRecords != 15 || r.Bounded.LedgerHeadSequence != 15 ||
		r.Bounded.UsageHeadSequence != 15 || r.Bounded.UsageRequests != 3 || r.Bounded.UsageAttempts != 3 ||
		r.Bounded.UsageCostMicrosUSD != 39 || r.Bounded.NativeFileBytes <= 0 ||
		r.Bounded.LedgerFileBytes <= 0 || r.Bounded.UsageFileBytes <= 0 ||
		r.FinalPhase != "authenticated native WAL and fresh Ledger/Usage disk projections open" {
		t.Fatalf("bounded fixture=%+v phase=%q", r.Bounded, r.FinalPhase)
	}
}

func TestBoundedProjectionModeRequiresOwnedDirectoryAndCeilings(t *testing.T) {
	base := options{mode: "bounded-projections", fixture: "priced", requests: 1, batch: 1, metadataWrites: 1, payloadBytes: 8, workers: 1}
	err := validateOptions(base)
	if err == nil || !strings.Contains(err.Error(), "requires -dir") {
		t.Fatalf("missing bounded projection limits accepted: %v", err)
	}
	base.directory, base.nativeMaxBytes, base.ledgerMaxBytes = t.TempDir(), 1024, 1
	err = validateOptions(base)
	if err == nil || !strings.Contains(err.Error(), "usage-max-bytes") {
		t.Fatalf("missing Usage disk ceiling accepted: %v", err)
	}
}

func TestLayoutPersistsBothLayouts(t *testing.T) {
	for _, mode := range []string{"layout-same", "layout-separate"} {
		r, err := runLayout(options{mode: mode, requests: 3, batch: 4, metadataWrites: 7, payloadBytes: 32, directory: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if r.Rows != 15 || r.Watermark != 15 || r.Metadata.Count != 7 || r.Projection.Count != 4 || r.FileBytes == 0 {
			t.Fatalf("layout=%+v", r)
		}
	}
}

func BenchmarkUsageWatermark(b *testing.B) {
	for _, requests := range []int{0, 1000, 10000} {
		aggregate := usage.NewAggregate()
		if err := visitFixture(requests, false, aggregate.Apply); err != nil {
			b.Fatal(err)
		}
		name := map[int]string{0: "0", 1000: "1000", 10000: "10000"}[requests]
		b.Run(name+"/Watermark", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if aggregate.Watermark().Sequence != uint64(requests*5) {
					b.Fatal("wrong watermark")
				}
			}
		})
		b.Run(name+"/Snapshot", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if aggregate.Snapshot().Watermark.Sequence != uint64(requests*5) {
					b.Fatal("wrong watermark")
				}
			}
		})
	}
}
