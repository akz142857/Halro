package usage

import (
	"context"
	"errors"
	"math"
)

// Fixed local admission only. Shared physical fees and capacity calibration are
// separate obligations. A permit covers the entire call, including file Close.
// ArchiveReaderSlots is the maximum number of concurrent archive operations;
// callers budgeting per-reader resources can size their envelopes against it.
const ArchiveReaderSlots = 4

func (e *Exporter) archiveReadError(ctx context.Context) error {
	if e.archiveRetired.Load() {
		return errors.Join(ctx.Err(), ErrDiskUsageClosed)
	}
	return ctx.Err()
}

func (e *Exporter) beginArchiveRead(ctx context.Context) (func() error, error) {
	if err := e.archiveReadError(ctx); err != nil {
		return nil, err
	}
	if !e.archiveLife.TryRLock() {
		return nil, ErrDiskUsageBusy
	}
	e.archiveReadersOnce.Do(func() { e.archiveReaders = make(chan struct{}, ArchiveReaderSlots) })
	select {
	case e.archiveReaders <- struct{}{}:
	default:
		e.archiveLife.RUnlock()
		return nil, ErrDiskUsageBusy
	}
	releaseRAM := func() error { return nil }
	if e.archiveRAMReserve != nil {
		var err error
		releaseRAM, err = e.archiveRAMReserve(ctx)
		if err != nil {
			<-e.archiveReaders
			e.archiveLife.RUnlock()
			return nil, err
		}
		if releaseRAM == nil {
			<-e.archiveReaders
			e.archiveLife.RUnlock()
			return nil, ErrDiskUsageCorrupt
		}
	}
	if err := e.archiveReadError(ctx); err != nil {
		<-e.archiveReaders
		e.archiveLife.RUnlock()
		return nil, errors.Join(err, releaseRAM())
	}
	return func() error {
		err := e.archiveReadError(ctx)
		<-e.archiveReaders
		e.archiveLife.RUnlock()
		return errors.Join(err, releaseRAM())
	}, nil
}

// This private exclusion gate confers no native/date deletion authority. The
// caller must establish that separately and publish its logical root before
// unlinking. Advance invalidates cursors before any reclamation publication;
// failure afterward may conservatively invalidate them, never restore them.
func (e *Exporter) withArchiveReclamation(ctx context.Context, work func(advance func() error) error) (err error) {
	if err := e.archiveReadError(ctx); err != nil {
		return err
	}
	if !e.mu.TryLock() {
		return ErrDiskUsageBusy
	}
	defer e.mu.Unlock()
	if !e.archiveLife.TryLock() {
		return ErrDiskUsageBusy
	}
	defer e.archiveLife.Unlock()
	if err := e.archiveReadError(ctx); err != nil {
		return err
	}
	advanced := false
	advance := func() error {
		if err := e.archiveReadError(ctx); err != nil {
			return err
		}
		if !advanced {
			if e.archiveEpoch == math.MaxUint64 {
				return ErrDiskUsageLimit
			}
			e.archiveEpoch++
			advanced = true
		}
		return nil
	}
	defer func() { err = errors.Join(err, e.archiveReadError(ctx)) }()
	return work(advance)
}

// Close permanently refuses new work, then joins accepted writers and actual
// readers before erasing keys/proofs. Canceling a read cannot shorten this join.
// The owner must keep its data-directory lock until Close returns.
func (e *Exporter) Close() error {
	e.archiveRetired.Store(true)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.archiveLife.Lock()
	defer e.archiveLife.Unlock()
	err := errors.Join(e.discardDateOrphanPages(), e.discardDateOrphans(), e.discardDateVerification(), e.discardDateReferences())
	clear(e.cursorKey[:])
	clear(e.datePolicyKey[:])
	e.datePolicyPrefix = nil
	e.datePolicyReady = false
	e.windowProof = nil
	e.windowPending = nil
	e.dateBuildReplay = nil
	e.dateReclaimReplay = nil
	e.dateDataProof = nil
	e.dateProof = nil
	// A failed Close may leave owned temporaries; keep the root counted so
	// no later Exporter of this process reclaims them.
	if err == nil && e.rootRegistered.CompareAndSwap(true, false) {
		unregisterArchiveRoot(e.root)
	}
	return err
}
