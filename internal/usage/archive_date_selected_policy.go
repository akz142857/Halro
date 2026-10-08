package usage

import (
	"context"
	"errors"
	"time"
)

// ReadAuthenticatedDateRetention selects a current date only from an
// independently keyed policy receipt and its authenticated native prefix. Root
// metadata alone never supplies permission to omit rows. No hint is written and
// the result grants no deletion or saved-projection authority.
func (e *Exporter) ReadAuthenticatedDateRetention(ctx context.Context, p *DiskProjection) (date time.Time, err error) {
	if p == nil {
		return date, ErrArchiveDateProof
	}
	if !e.mu.TryLock() {
		return date, ErrDiskUsageBusy
	}
	defer e.mu.Unlock()
	defer func() {
		err = errors.Join(err, e.archiveReadError(ctx))
		if p.retired.Load() {
			err = errors.Join(err, ErrDiskUsageClosed)
		}
		if err != nil {
			date = time.Time{}
		}
	}()
	if err := e.archiveReadError(ctx); err != nil {
		return date, err
	}
	stats, err := p.ReadStats(ctx)
	if err != nil {
		return date, err
	}
	root, err := openArchiveManifest(e.root)
	if err != nil {
		return date, err
	}
	defer func() { err = errors.Join(err, root.check(), root.file.Close()) }()
	current, _, err := root.walk(ctx, nil)
	if err != nil {
		return date, err
	}
	if current.Retention == (ArchiveDateRetention{}) {
		return date, nil
	}
	policy, err := e.readCurrentDatePolicy(ctx, current, stats.Watermark)
	if err != nil {
		return date, err
	}
	defer func() {
		err = errors.Join(err, checkDateBuildState(policy.file, policy.info, policy.path), policy.file.Close())
	}()
	return time.Parse("2006-01-02", current.Retention.RetainFromDate)
}
