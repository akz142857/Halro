package governance

import (
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/nativephysical"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"io"
)

// PhysicalBatchBytes covers every legal encoded batch, including all frames.
const PhysicalBatchBytes int64 = MaxBatchPayloadBytes + MaxBatchEvents*(frameHeaderSize+frameMACSize)

// PhysicalBatchRAMBytes is the fee for the one batch encoder. A maximum batch
// allocates about 25 MiB in total (payload marshal, the exact frame buffer and
// transient garbage); TestGovernanceHAMaximumBatchAllocationFitsPhysicalBatchFee guards it.
const PhysicalBatchRAMBytes int64 = 32 << 20

type HAPhysicalOptions struct {
	NativeDisk, ControlDisk resourcebudget.Permit
	WindowBytes             int64
	Restart                 bool
	phase                   func(string) error
}

// PrepareHAPhysicalNative can only use this actual selected HA constructor,
// original Manager, Guard, independently selected common seal and current role.
// All native bytes are authenticated on the original writer under its mutex.
// This allocation receipt grants no confirmation, projection or HA Ready.
func (l *Log) PrepareHAPhysicalNative(ctx context.Context, budget *resourcebudget.Manager, o HAPhysicalOptions) error {
	if l == nil {
		return ErrStartupCache
	}
	l.mu.Lock()
	var h *haSelectedStartupProvenance
	if l.startupProvenance != nil {
		h = l.startupProvenance.ha
	}
	l.mu.Unlock()
	if h == nil {
		return ErrStartupCache
	}
	return h.withScope(ctx, func(ctx context.Context, a authenticatedstate.SealEligibility, _ HASelectedNativePosition) error {
		l.mu.Lock()
		defer l.mu.Unlock()
		p := l.startupProvenance
		if p == nil || p.ha != h || budget == nil || l.cacheBudget != budget || !l.cacheOwned || l.file == nil || l.closing || l.closed || l.freezeCancel != nil || l.terminalErr != nil || l.physical != nil || !selectedStartupModeValid(ctx, Options{Replica: l.replica, BeforeDurable: l.beforeDurable, AfterDurable: l.afterDurable}, a, h) || !p.guard.CoversNativePath(l.path) || len(l.key) != 32 {
			return ErrStartupCache
		}
		if l.startupAudit == nil || !l.startupAudit.Snapshot().Ready {
			return ErrStartupAuditPending
		}
		if err := p.publisher.ValidateOwner(p.guard, budget); err != nil {
			return err
		}
		seal, found, err := p.publisher.SelectContext(ctx, a)
		if err != nil || !found || seal != p.seal {
			return errors.Join(ErrStartupCache, err)
		}
		head := l.physicalHeadLocked()
		if err := l.authenticatePhysicalPrefixLocked(ctx, head); err != nil {
			return err
		}
		// Install before any allocation so uncertain creation/IO retains its owner.
		l.physical, err = nativephysical.Prepare(ctx, l.path, l.file, l.key, head, nativephysical.Options{Manager: budget, NativeDisk: o.NativeDisk, ControlDisk: o.ControlDisk, MaxDiskBytes: l.maxDiskBytes, AppendBytes: PhysicalBatchBytes, WindowBytes: o.WindowBytes, RAMBytes: PhysicalBatchRAMBytes, Identity: p.identity, Restart: o.Restart, Authenticate: l.authenticatePhysicalPrefixLocked, Phase: o.phase})
		if err != nil && l.physical != nil {
			l.terminalErr = err
		}
		return err
	})
}
func (l *Log) physicalHeadLocked() nativephysical.Position {
	return nativephysical.Position{Sequence: l.sequence, Hash: l.lastHash, EOF: l.offset}
}
func (l *Log) authenticatePhysicalPrefixLocked(ctx context.Context, h nativephysical.Position) error {
	if h.EOF < 0 || h.EOF > l.offset || h.Sequence > l.sequence {
		return ErrCorrupt
	}
	actual, partial, err := scanFrom(io.NewSectionReader(l.file, 0, h.EOF), l.key, Summary{}, func(Record) error { return ctx.Err() })
	if err != nil || partial || actual.Records != h.Sequence || actual.Bytes != h.EOF || actual.LastHash != h.Hash {
		return errors.Join(ErrCorrupt, err)
	}
	return ctx.Err()
}

// The encoder is acquired before payload graphs exist. Registration under the
// actual native mutex lets Close withdraw new encoders and join accepted ones.
func (l *Log) withPhysicalEncoder(ctx context.Context, visit func(context.Context) error) error {
	l.mu.Lock()
	w := l.physical
	if w == nil {
		l.mu.Unlock()
		return visit(ctx)
	}
	if l.closing || l.closed || l.file == nil {
		l.mu.Unlock()
		return ErrStartupCache
	}
	l.mu.Unlock()
	return w.WithEncoder(ctx, func(ctx context.Context) error {
		job, cancel := context.WithCancel(ctx)
		l.mu.Lock()
		if l.closing || l.closed {
			l.mu.Unlock()
			cancel()
			return ErrStartupCache
		}
		l.physicalCancel = cancel
		l.readerWG.Add(1)
		l.mu.Unlock()
		defer func() {
			cancel()
			l.mu.Lock()
			l.physicalCancel = nil
			l.mu.Unlock()
			l.readerWG.Done()
		}()
		return visit(job)
	})
}
func (l *Log) physicalBeforeWriteLocked(size int64, count int) (*nativephysical.Reservation, error) {
	if l.physical == nil {
		return nil, nil
	}
	r, err := l.physical.Reserve(l.physicalHeadLocked(), size)
	if err != nil {
		if errors.Is(err, ownedfile.ErrReplaced) || errors.Is(err, ownedfile.ErrRetirement) || errors.Is(err, nativephysical.ErrOwnership) {
			l.terminalErr = err
		}
		return nil, err
	}
	if l.physicalProjection != nil {
		if err := l.physicalProjection.reserveNativeLocked(count); err != nil {
			return nil, errors.Join(err, r.Release())
		}
	}
	if err = r.BeforeWrite(l.physicalHeadLocked(), size); err != nil {
		if l.physicalProjection != nil {
			l.physicalProjection.rollbackNativeLocked(count)
		}
		return nil, errors.Join(err, r.Release())
	}
	return r, nil
}
