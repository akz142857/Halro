package usage

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// ReadPendingDatePolicy selects an authenticated, not-yet-published intent's
// date so the periodic controller finishes it before an archive append changes
// its exact old root. It supplies no publication or deletion permission.
func (e *Exporter) ReadPendingDatePolicy(ctx context.Context, p *DiskProjection) (pending time.Time, err error) {
	if p == nil {
		return pending, ErrArchiveDateProof
	}
	if !e.mu.TryLock() {
		return pending, ErrDiskUsageBusy
	}
	defer e.mu.Unlock()
	if err := e.archiveReadError(ctx); err != nil {
		return pending, err
	}
	stats, err := p.ReadStats(ctx)
	if err != nil {
		return pending, err
	}
	state, _, file, _, err := readDateActivationState(ctx, filepath.Join(e.root, archiveDateActivationName))
	if errors.Is(err, os.ErrNotExist) {
		return pending, nil
	}
	if err != nil {
		return pending, err
	}
	defer func() {
		err = errors.Join(err, file.Close(), e.archiveReadError(ctx))
		if err != nil {
			pending = time.Time{}
		}
	}()
	if err := e.verifyDatePolicy(ctx, state, stats.Watermark); err != nil {
		return pending, err
	}
	root, err := openArchiveManifest(e.root)
	if err != nil {
		return pending, err
	}
	defer func() { err = errors.Join(err, root.file.Close()) }()
	position, digest, err := root.walk(ctx, nil)
	if err != nil {
		return pending, err
	}
	_, candidate, err := state.positions()
	if err != nil {
		return pending, err
	}
	if candidate.Retention == position.Retention && candidate.LastSequence <= position.LastSequence {
		return pending, root.check()
	}
	if digest != sha256.Sum256(state.OldRoot) {
		return pending, ErrDiskUsageBusy
	}
	if position.Retention != (ArchiveDateRetention{}) {
		previous, err := e.readDateCompletion(ctx, position, stats.Watermark)
		if err != nil {
			return pending, err
		}
		defer func() { err = errors.Join(err, previous.file.Close()) }()
		if err := e.verifyDateSuccession(ctx, state, previous.state, previous.raw, stats.Watermark); err != nil {
			return pending, err
		}
		if err := previous.check(ctx); err != nil {
			return pending, err
		}
	}
	pending, err = time.Parse("2006-01-02", candidate.Retention.RetainFromDate)
	return pending, errors.Join(err, root.check())
}
