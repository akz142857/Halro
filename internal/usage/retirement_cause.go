package usage

import (
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

type usageRetirementCause struct{ err error }

// The first failed actual reducer job remains observable after admission is
// withdrawn. Concurrent readers must not replace it with an unrelated closed
// error or race the job's protected cleanup state. This diagnostic grants no
// readiness, repair or resource-refund authority.
func (p *DiskProjection) retireBecause(err error) {
	if err != nil {
		p.retirementCause.CompareAndSwap(nil, &usageRetirementCause{err})
	}
	p.retired.Store(true)
}
func (p *DiskProjection) unavailableError(stage string) error {
	if p != nil {
		if cause := p.retirementCause.Load(); cause != nil {
			return errors.Join(fmt.Errorf("Usage %s unavailable: %w", stage, ErrDiskUsageClosed), cause.err)
		}
	}
	return ErrDiskUsageClosed
}

// capacityStop reports a reduction stopped because the page pack refused its
// next update before appending anything. Every earlier update committed whole
// with its watermark, so the projection is consistent where it stopped and the
// next catch-up resumes there; the pack needs reclamation, not recovery.
func (p *DiskProjection) capacityStop(err error) bool {
	return errors.Is(err, authenticatedstate.ErrCapacity) && p != nil && p.engine != nil && p.engine.cow != nil && p.engine.cow.NativeFrameRecoveryDebt() == nil
}

// PackUsage reports the typed page pack's use; see authenticatedstate.Store.
// ok is false while a reduction holds the projection or it has no page pack.
func (p *DiskProjection) PackUsage() (used, usable uint64, ok bool) {
	if p == nil || !p.mu.TryLock() {
		return 0, 0, false
	}
	defer p.mu.Unlock()
	if p.retired.Load() || p.engine == nil || p.engine.cow == nil {
		return 0, 0, false
	}
	used, usable, ok = p.engine.cow.PackUsage()
	if ok {
		p.pack.used.Store(used)
		p.pack.usable.Store(usable)
	}
	return used, usable, ok
}

// usagePackObservation keeps the last observed page-pack use and the largest
// bytes one reduced record has appended, for admission that must not lock.
type usagePackObservation struct {
	used, usable, recordBytes, applied atomic.Uint64
}

func (o *usagePackObservation) reduced(store *authenticatedstate.Store, appended int64, records, through uint64) {
	if appended > 0 && records > 0 {
		per := (uint64(appended) + records - 1) / records
		for {
			old := o.recordBytes.Load()
			if per <= old || o.recordBytes.CompareAndSwap(old, per) {
				break
			}
		}
	}
	o.applied.Store(through)
	if used, usable, ok := store.PackUsage(); ok {
		o.used.Store(used)
		o.usable.Store(usable)
	}
}

// PackAdmission reports the page pack's use, refreshed when no reduction holds
// the projection and otherwise as last observed, the largest bytes one reduced
// record has appended and the last reduced Ledger sequence. Zero recordBytes
// means no reduction has been observed yet. It never waits.
func (p *DiskProjection) PackAdmission() (used, usable, recordBytes, applied uint64) {
	if p == nil {
		return 0, 0, 0, 0
	}
	p.PackUsage()
	return p.pack.used.Load(), p.pack.usable.Load(), p.pack.recordBytes.Load(), p.pack.applied.Load()
}
