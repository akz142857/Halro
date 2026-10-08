package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/usage"
)

var errReplicaAccountingRetired = errors.New("Replica accounting generation is retired")

// A candidate owns one fresh disk Ledger, one Usage derivative and one adapter.
// Metadata/native remain caller-owned. This is startup/maintenance preparation,
// not permission to select a default owner, publish readiness or trust a saved
// checkpoint. Caller retains exclusive startup/maintenance and resource admission.
type replicaAccountingGeneration struct {
	retirementErr   error
	mu              sync.RWMutex
	retired         atomic.Bool
	closed          bool
	closeErr        error
	ledger          *ledger.LedgerProjection
	usage           *usage.LedgerDerivative
	usageDisk       *usage.DiskProjection
	usageOutput     string
	projection      *replication.NativeProjection
	prefix          ledger.ProjectionPrefix
	member          replication.MemberState
	output          string
	diskPermit      *runtimeProjectionDiskPermit
	usageDiskPermit *runtimeProjectionDiskPermit
}

func (r *replicationRuntime) prepareReplicaAccountingGeneration(ctx context.Context, native *ledger.Log, metadata *boltstore.Store, output string) (result *replicaAccountingGeneration, resultErr error) {
	return r.prepareReplicaAccountingGenerationWithDisk(ctx, native, metadata, output, 0, nil)
}

func (r *replicationRuntime) prepareReplicaAccountingGenerationWithDisk(ctx context.Context, native *ledger.Log, metadata *boltstore.Store, output string, maxDiskBytes int64, diskPermit *runtimeProjectionDiskPermit) (result *replicaAccountingGeneration, resultErr error) {
	return r.prepareReplicaAccountingGenerationWithDiskLimits(ctx, native, metadata, output, maxDiskBytes, diskPermit, 0, nil)
}

func (r *replicationRuntime) prepareReplicaAccountingGenerationWithDiskLimits(ctx context.Context, native *ledger.Log, metadata *boltstore.Store, output string, maxDiskBytes int64, diskPermit *runtimeProjectionDiskPermit, usageMaxDiskBytes int64, usageDiskPermit *runtimeProjectionDiskPermit) (result *replicaAccountingGeneration, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, diskPermit.Release(), usageDiskPermit.Release())
	}
	if r == nil || r.publisher == nil || metadata == nil {
		return nil, errors.Join(errors.New("Replica generation requires member and Metadata owners"), diskPermit.Release(), usageDiskPermit.Release())
	}
	g := &replicaAccountingGeneration{output: output, diskPermit: diskPermit, usageDiskPermit: usageDiskPermit}
	complete := false
	defer func() {
		if !complete {
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				g.retirementErr = errors.Join(g.retirementErr, resultErr)
				g.closeErr = errors.Join(g.closeErr, resultErr)
			}
			resultErr = errors.Join(resultErr, g.Close())
		}
	}()
	before := r.publisher.Snapshot()
	initialMetadata, err := metadata.ReadMetadataAppliedPosition(ctx)
	if err != nil {
		return nil, err
	}
	checkMetadata := func() error {
		position, err := metadata.ReadMetadataAppliedPosition(ctx)
		if err != nil {
			return err
		}
		if position.Epoch != before.Projection.MetadataEpoch || position.Sequence != before.Projection.MetadataSequence || position != initialMetadata {
			return errors.New("Replica Metadata does not match generation applied cut")
		}
		return nil
	}
	if err := checkMetadata(); err != nil {
		return nil, err
	}
	p, prefix, err := r.buildReplicaLedgerOwnerWithLimit(ctx, native, output, maxDiskBytes, func(candidate *ledger.LedgerProjection) {
		g.ledger = candidate
	})
	if err != nil {
		return nil, err
	}
	g.prefix, g.member = prefix, before
	fail := func(err error) (*replicaAccountingGeneration, error) {
		if errors.Is(err, ownedfile.ErrRetirement) {
			g.retirementErr = errors.Join(g.retirementErr, err)
			g.closeErr = errors.Join(g.closeErr, err)
		}
		return nil, errors.Join(err, g.Close())
	}
	g.usageOutput = output + ".usage"
	g.usage, g.usageDisk, err = usage.NewDiskLedgerDerivativeWithLimit(p, g.usageOutput, usageMaxDiskBytes)
	if err != nil {
		return fail(err)
	}
	// Local Usage checkpoints/rollups are not authenticated role authorization.
	// Build a fresh candidate without reading/resetting those shared Metadata rows.
	// Full native authentication still includes durable facts excluded from Apply.
	collector, err := usage.NewProjectedCollector(g.usage, func(replayCtx context.Context, from ledger.Watermark, visit func(ledger.Record) error) (ledger.Watermark, error) {
		_, replayErr := native.ReplayAuthenticated(replayCtx, ledger.ReplayAnchor{}, func(record ledger.Record) error {
			position := ledger.Watermark{Generation: record.Generation, Sequence: record.Sequence, Offset: record.Offset}
			if position.After(prefix.Head) || !position.After(from) {
				return nil
			}
			return visit(record)
		})
		if replayErr != nil {
			return ledger.Watermark{}, replayErr
		}
		return prefix.Head, nil
	}, 1)
	if err != nil {
		return fail(err)
	}
	err = collector.CatchUp(ctx)
	// Join and relinquish this construction writer before the live native
	// adapter takes ownership. No legacy Aggregate or concurrent second writer.
	err = errors.Join(err, collector.Retire(context.Background()))
	if err != nil {
		return fail(err)
	}
	stats, err := g.usageDisk.ReadStats(ctx)
	if err != nil {
		return fail(err)
	}
	wm := stats.Watermark
	if wm.Sequence != prefix.Head.Sequence || wm.After(prefix.Head) {
		return fail(errors.New("Replica Usage does not reach permitted semantic prefix"))
	}
	head, hash, err := p.ReadPosition(ctx)
	if err != nil {
		return fail(err)
	}
	if head != prefix.Head || hash != prefix.Hash {
		return fail(errors.New("Replica Ledger candidate changed during Usage construction"))
	}
	if err := checkMetadata(); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if !sameReplicaOwnerBuildCut(before, r.publisher.Snapshot()) {
		return fail(errors.New("Replica generation identity or applied cut changed during construction"))
	}
	g.projection, err = replication.NewNativeProjectionWithLedgerOwner(native, p, metadata, g.usage.Apply)
	if err != nil {
		return fail(err)
	}
	if g.diskPermit != nil {
		if err := g.diskPermit.Reclassify(resourcebudget.Background); err != nil {
			return fail(err)
		}
	}
	if g.usageDiskPermit != nil {
		if err := g.usageDiskPermit.Reclassify(resourcebudget.Background); err != nil {
			return fail(err)
		}
	}
	complete = true
	return g, nil
}

// All applier mutations/validation run through this owner ticket, so retirement
// joins the entire adapter call, not just a DB transaction or Usage callback.
func (g *replicaAccountingGeneration) withProjection(ctx context.Context, fn func(*replication.NativeProjection) error) error {
	if g.retired.Load() {
		return errReplicaAccountingRetired
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.retired.Load() || g.projection == nil {
		return errReplicaAccountingRetired
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(g.projection)
}
func (g *replicaAccountingGeneration) ApplyLedgerThrough(ctx context.Context, generation, sequence uint64) error {
	return g.withProjection(ctx, func(p *replication.NativeProjection) error { return p.ApplyLedgerThrough(ctx, generation, sequence) })
}
func (g *replicaAccountingGeneration) ApplyLedgerBatchThrough(ctx context.Context, generation, sequence uint64) error {
	return g.withProjection(ctx, func(p *replication.NativeProjection) error {
		return p.ApplyLedgerBatchThrough(ctx, generation, sequence)
	})
}
func (g *replicaAccountingGeneration) ApplyMetadataThrough(ctx context.Context, generation, sequence uint64) error {
	return g.withProjection(ctx, func(p *replication.NativeProjection) error { return p.ApplyMetadataThrough(ctx, generation, sequence) })
}
func (g *replicaAccountingGeneration) ApplyLedgerRoll(ctx context.Context, roll replication.LedgerRollMetadata) error {
	return g.withProjection(ctx, func(p *replication.NativeProjection) error { return p.ApplyLedgerRoll(ctx, roll) })
}
func (g *replicaAccountingGeneration) ValidateLedgerForPublication(ctx context.Context) error {
	return g.withProjection(ctx, func(p *replication.NativeProjection) error { return p.ValidateLedgerForPublication(ctx) })
}
func (g *replicaAccountingGeneration) ValidateSchemaBoundary(ctx context.Context, from, to uint32) error {
	return g.withProjection(ctx, func(p *replication.NativeProjection) error { return p.ValidateSchemaBoundary(ctx, from, to) })
}

// Close permanently withdraws adapter calls, joins them, retires Usage, closes
// the exclusively created derived DB and removes only that owned path. A cleanup
// failure is returned, never interpreted as disk space release. No native/shared
// Metadata resource is closed. Do not invoke from a generation-owned callback.
func (g *replicaAccountingGeneration) Close() error {
	if g == nil {
		return nil
	}
	g.retired.Store(true)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return g.closeErr
	}
	g.closeErr = errors.Join(g.closeErr, g.retirementErr)
	g.closed = true
	g.projection = nil
	if g.usage != nil {
		g.usage.Close()
		g.usage = nil
	}
	if g.usageDisk != nil {
		discardErr := g.usageDisk.Discard()
		g.closeErr = errors.Join(g.closeErr, discardErr)
		g.usageDisk = nil
		if discardErr == nil && g.usageDiskPermit != nil && g.retirementErr == nil {
			if err := g.usageDiskPermit.Release(); err != nil {
				g.closeErr = errors.Join(g.closeErr, err)
			} else {
				g.usageDiskPermit = nil
			}
		}
	} else if g.usageDiskPermit != nil && g.retirementErr == nil {
		removed, err := derivedFileRemoved(g.usageOutput)
		g.closeErr = errors.Join(g.closeErr, err)
		if err == nil && removed {
			if err := g.usageDiskPermit.Release(); err != nil {
				g.closeErr = errors.Join(g.closeErr, err)
			} else {
				g.usageDiskPermit = nil
			}
		} else if err == nil {
			g.closeErr = errors.Join(g.closeErr, errors.New("Replica Usage file remains; retain its Disk claim"))
		}
	}
	ledgerRemoved := false
	if g.ledger != nil {
		discardErr := g.ledger.Discard()
		g.closeErr = errors.Join(g.closeErr, discardErr)
		ledgerRemoved = discardErr == nil
		g.ledger = nil
	} else {
		var err error
		ledgerRemoved, err = derivedFileRemoved(g.output)
		g.closeErr = errors.Join(g.closeErr, err)
		if err == nil && !ledgerRemoved {
			g.closeErr = errors.Join(g.closeErr, errors.New("Replica Ledger file remains; retain its Disk claim"))
		}
	}
	if ledgerRemoved && g.diskPermit != nil && g.retirementErr == nil {
		if err := g.diskPermit.Release(); err != nil {
			g.closeErr = errors.Join(g.closeErr, err)
		} else {
			g.diskPermit = nil
		}
	}
	return g.closeErr
}
