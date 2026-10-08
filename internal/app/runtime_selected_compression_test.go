package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestSelectedRuntimeCompressionReopensOldCommonRootAndPublishesPhysicalLocator(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	legacy, credentials, _ := openBootstrappedRuntimeForModel(t, "gpt-test")
	cfg, instance := legacy.config, legacy.instanceID
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	cfg = activateClosedRuntimeMemoryStateFixture(t, ctx, cfg, instance, false)
	opts := OpenOptions{AllowGeneratedSetupToken: true, resourceSampleSource: pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := OpenWithOptions(ctx, cfg, logger, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r == nil {
			return
		}
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	q, err := r.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, "compress_selected_business", "chat")
	if err != nil {
		t.Fatal(err)
	}
	a, err := r.accounting.ReserveAttempt(ctx, q, 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.accounting.Settle(ctx, a, budget.Settlement{CommittedMicrosUSD: 7, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	if err := r.accounting.Finalize(ctx, q, "success"); err != nil {
		t.Fatal(err)
	}
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	roll, err := r.ledger.Roll()
	if err != nil || !roll.Rolled {
		t.Fatal(err)
	}
	from, readErr := r.selectedState.generation.Owners.Ledger.ReadReplayAnchor(ctx)
	head, hash, _ := r.ledger.ChainHead()
	if readErr != nil || from.Head.Sequence != head.Sequence || from.Hash != hash || head.Offset != 0 || head.Generation <= from.Head.Generation {
		t.Fatal("actual physical Roll changed logical business position", readErr)
	}
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal("post-Roll actual Collector catch-up", err)
	}
	if err := r.CheckpointMemoryState(ctx); err != nil {
		t.Fatal("actual common selection before physical compression", err)
	}
	before := r.selectedState.generation.Seal
	if _, err := r.ledger.Compact(roll.Sealed.Generation); err != nil {
		t.Fatal(err)
	}
	if r.selectedState.generation.Seal != before {
		t.Fatal("physical compression invented common authority")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	drained := r.selectedState.generation.Seal
	if r.backgroundWait.drainState.Load() != businessDrainComplete || drained.Revision != before.Revision+1 {
		t.Fatal("actual terminal drain did not publish its complete successor", drained)
	}
	r, err = OpenWithOptions(ctx, cfg, logger, opts)
	if err != nil {
		t.Fatal("ordinary seven-root reopen after compaction", err)
	}
	if r.selectedState.generation.Seal != drained {
		t.Fatal("reopen did not select the actual drained common authority")
	}
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err := r.usage.disk.ReadStats(ctx)
	if err != nil || stats.Totals.Requests != 1 || stats.Totals.CostMicrosUSD != 7 || stats.Metrics.ActiveRequests != 0 {
		t.Fatal("compaction changed actual business facts", stats, err)
	}
	if err := r.CheckpointMemoryState(ctx); err != nil {
		t.Fatal("ordinary common publication of physical locator", err)
	}
	if r.selectedState.generation.Seal.Revision <= before.Revision {
		t.Fatal("common publication did not advance")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = OpenWithOptions(ctx, cfg, logger, opts)
	if err != nil {
		t.Fatal("published physical locator cannot reopen", err)
	}
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, "compress_selected_after_reopen", "chat"); err != nil {
		t.Fatal("actual business admission after physical publication", err)
	}
}

func TestSelectedRuntimeSeventyLiveRollsAndCompressionUseFiniteDirectoryPages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	legacy, credentials, _ := openBootstrappedRuntimeForModel(t, "gpt-test")
	cfg, instance := legacy.config, legacy.instanceID
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	cfg = activateClosedRuntimeMemoryStateFixture(t, ctx, cfg, instance, false)
	// Seventy retained gzip generations each reserve a conservative >1 MiB
	// output envelope, plus manifests and locators. Declare that capacity; the
	// small 64 MiB functional default must correctly refuse this workload.
	limits := *cfg.ResourceBudget
	limits.LedgerAppendDiskBytes = 128 << 20
	cfg.ResourceBudget = &limits
	opts := OpenOptions{AllowGeneratedSetupToken: true, resourceSampleSource: pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })}
	r, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r == nil {
			return
		}
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	before := r.selectedState.generation.Seal
	for i := 0; i < 70; i++ {
		q, err := r.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, fmt.Sprintf("live_rolled_%d", i), "chat")
		if err != nil {
			t.Fatal(i, err)
		}
		if err := r.accounting.Finalize(ctx, q, "success"); err != nil {
			t.Fatal(i, err)
		}
		if err := r.usageCollector.CatchUp(ctx); err != nil {
			t.Fatal(i, err)
		}
		roll, err := r.ledger.Roll()
		if err != nil || !roll.Rolled {
			t.Fatal(i, roll, err)
		}
	}
	if err := r.usageCollector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.CheckpointMemoryState(ctx); err != nil {
		t.Fatal("live 70-generation publication", err)
	}
	ref, err := r.selectedState.generation.Owners.Directory.SealReference(ctx)
	if err != nil || ref.Root.Rows != 71 || ref.Position.Generation != 71 {
		t.Fatal("directory lost rolled descriptors", ref, err)
	}
	if r.selectedState.generation.Seal.Revision <= before.Revision {
		t.Fatal("common authority did not advance")
	}
	for generation := uint64(1); generation <= 70; generation++ {
		if _, err := r.ledger.Compact(generation); err != nil {
			t.Fatal(generation, err)
		}
	}
	if err := r.CheckpointMemoryState(ctx); err != nil {
		t.Fatal("live 70 compressed descriptor refresh", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err := r.usage.disk.ReadStats(ctx)
	if err != nil || stats.Totals.Requests != 70 || stats.Metrics.ActiveRequests != 0 {
		t.Fatal("live maintenance changed business facts", stats, err)
	}
	for generation := uint64(1); generation <= 70; generation++ {
		segment, found, err := r.selectedState.generation.Owners.Directory.ReadSegment(ctx, generation)
		if err != nil || !found || !segment.Compressed {
			t.Fatal("published physical locator differs", generation, segment, err)
		}
	}
}
