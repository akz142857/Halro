package usage

import (
	"errors"
	"fmt"
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
