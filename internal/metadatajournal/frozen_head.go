package metadatajournal

import (
	"context"
	"errors"
	"io"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

// ValidateFrozenStandalone requires a live actual freeze with no replication
// owner or hook. It is meaningful only inside a WithFrozenHead callback.
func (l *Log) ValidateFrozenStandalone() error {
	if l == nil {
		return ErrCorrupt
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.freezeCancel == nil || l.availabilityError() != nil || !l.selectedStartupStructuralReadyLocked() || l.replica || l.readOnly || l.afterDurable != nil || l.beforeDurable != nil || l.waitConfirmed != nil || l.closed || l.closing || l.terminalErr != nil {
		return errors.Join(ErrCorrupt, l.terminalErr)
	}
	return nil
}

const FrozenReadRAMBytes int64 = 32 << 20

var ErrNativeFrozen = errors.New("metadata native mutation is frozen by a read job")

// WithFrozenHead excludes all Log-owned native append/trim/hook changes and
// authenticates the complete actual descriptor before and after visit. Reads
// may reenter the Log; Close cancels visit and joins it before closing the FD.
// The callback must not Close/Discard this Log, and its context must not escape.
// A format1 head authenticates only journal bytes, never its live DB baseline;
// this job grants no Metadata migration, identity, role or common-cut authority.
func (l *Log) WithFrozenHead(ctx context.Context, budget *resourcebudget.Manager, visit func(context.Context, Head) error) (resultErr error) {
	if l == nil || budget == nil || visit == nil {
		return ErrCorrupt
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	credit, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: FrozenReadRAMBytes})
	if err != nil {
		return err
	}
	opCtx, cancel := context.WithCancel(ctx)
	l.mu.Lock()
	if l.startupAudit != nil && !l.selectedStartupStructuralReadyLocked() {
		l.mu.Unlock()
		cancel()
		return errors.Join(ErrStartupAuditPending, credit.Release())
	}
	if err := l.descriptorAvailabilityError(); err != nil || l.terminalErr != nil || l.freezeCancel != nil {
		failure := errors.Join(err, l.terminalErr)
		if l.freezeCancel != nil {
			failure = ErrNativeFrozen
		}
		l.mu.Unlock()
		cancel()
		return errors.Join(failure, credit.Release())
	}
	frozen := Head{Epoch: l.epoch, Sequence: l.sequence, Offset: l.offset, Hash: l.lastHash, TrimmedThrough: l.trimmedThrough, FormatVersion: l.formatVersion, FileRevision: l.fileRevision, Baseline: l.baseline}
	l.freezeCancel = cancel
	l.freezeWG.Add(1)
	l.mu.Unlock()
	defer l.freezeWG.Done()
	defer func() {
		cancel()
		l.mu.Lock()
		l.freezeCancel = nil
		l.mu.Unlock()
		if err := credit.Release(); err != nil {
			resultErr = errors.Join(resultErr, err)
			l.mu.Lock()
			l.freezeDebt = credit
			l.terminalErr = errors.Join(l.terminalErr, err)
			l.mu.Unlock()
		}
	}()
	validate := func() error {
		l.mu.Lock()
		if err := opCtx.Err(); err != nil {
			l.mu.Unlock()
			return err
		}
		if err := l.descriptorAvailabilityError(); err != nil {
			l.mu.Unlock()
			return err
		}
		err := l.checkPreparedFile()
		file, key := l.file, l.key
		l.mu.Unlock()
		if err == nil {
			// The actual freeze owns the FD/key through the whole job. ReadAt
			// avoids shared seek state while allowing Close to revoke its context.
			actual, partial, scanErr := scanContext(opCtx, io.NewSectionReader(file, 0, frozen.Offset), key, nil)
			err = scanErr
			if err == nil && (partial || actual != frozen) {
				err = ErrCorrupt
			}
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		if err == nil {
			err = l.checkPreparedFile()
		}
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			l.terminalErr = errors.Join(l.terminalErr, err)
		}
		return errors.Join(err, opCtx.Err())
	}
	if err := validate(); err != nil {
		return err
	}
	callbackErr := visit(opCtx, frozen)
	if err := opCtx.Err(); err != nil {
		return errors.Join(callbackErr, err)
	}
	return errors.Join(callbackErr, validate(), opCtx.Err())
}
