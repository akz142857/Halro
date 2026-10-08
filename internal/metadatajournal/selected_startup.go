package metadatajournal

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

var ErrStartupAuditPending = errors.New("selected Metadata startup requires historical audit before writes or readiness")

// SelectedSealOpenOptions borrows the actual directory guard and publisher.
// Authority and Binding must come from independent startup validation. The
// publisher authenticates their exact common position; no native header or
// local latest-root pointer selects authority. This first profile accepts only
// untrimmed format2 revision1 Standalone journals, never HA or epoch transitions.
type SelectedSealOpenOptions struct {
	ReadOnly  bool
	Publisher *authenticatedstate.SealCandidatePublisher
	Guard     *lock.Lock
	Authority authenticatedstate.SealEligibility
	Budget    *resourcebudget.Manager
	Binding   BaselineBinding
	MaxBytes  int64
}

type NativeStartupReceipt struct {
	Saved, Head            Head
	DecodedRecords         uint64
	HistoricalAuditPending bool
}

// OpenBoundWithSelectedSeal authenticates the bounded epoch header and only
// the suffix following the selected common Metadata cut. It never repairs a
// partial tail. The returned actual Log refuses mutation and HealthError until
// AuditSelectedStartup checks all historical bytes and the saved boundary.
// This explicit primitive neither opens the Metadata root nor enables Runtime.
func OpenBoundWithSelectedSeal(ctx context.Context, path string, key []byte, o SelectedSealOpenOptions) (result *Log, receipt NativeStartupReceipt, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, receipt, err
	}
	if !filepath.IsAbs(path) || len(key) != KeySize || o.Publisher == nil || o.Guard == nil || o.Budget == nil || !o.Binding.valid() || o.MaxBytes <= 0 || o.Authority.Role != authenticatedstate.SealStandalone || o.Authority.Coverage != authenticatedstate.SealCoverageAuthenticated || !o.Guard.CoversNativePath(path) {
		return nil, receipt, ErrCorrupt
	}
	if err := o.Publisher.ValidateOwner(o.Guard, o.Budget); err != nil {
		return nil, receipt, err
	}
	selected, found, err := o.Publisher.Select(o.Authority)
	if err != nil || !found {
		return nil, receipt, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	var position authenticatedstate.SealPosition
	var root authenticatedstate.SealRoot
	for _, p := range selected.Positions[:selected.PositionCount] {
		if p.Component == authenticatedstate.SealComponentMetadata {
			position = p
		}
	}
	for _, r := range selected.Roots[:selected.RootCount] {
		if r.Kind == authenticatedstate.SealRootState && r.Component == authenticatedstate.SealComponentMetadata && r.IndexID == 1 && r.Codec == 2 {
			root = r
		}
	}
	if position.Epoch != o.Binding.Epoch || position.Revision != 1 || position.Generation != 0 || position.Offset == 0 || position.Offset > math.MaxInt64 || position.Offset > uint64(o.MaxBytes) || position.Hash == ([32]byte{}) || position.MetadataBaseline != o.Binding.Cut || root.State.Scope != o.Binding.Scope || root.Rows == 0 || root.Empty || root.State.Revision == 0 || root.FileID == ([16]byte{}) {
		return nil, receipt, ErrCorrupt
	}
	saved := Head{Epoch: position.Epoch, Sequence: position.Sequence, Offset: int64(position.Offset), Hash: position.Hash, FormatVersion: boundFrameVersion, FileRevision: 1, Baseline: o.Binding}
	work, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: FrozenReadRAMBytes})
	if err != nil {
		return nil, receipt, err
	}
	var file *os.File
	defer func() {
		if result == nil && file != nil {
			if closeErr := closeDescriptor(file); closeErr != nil {
				resultErr = errors.Join(resultErr, closeErr)
				result = &Log{readOnly: o.ReadOnly, file: file, path: path, key: append([]byte(nil), key...), closed: true, retirementErr: closeErr, closeErr: resultErr, startupAuditPending: true, startupDebt: work}
				return
			}
		}
		if releaseErr := work.Release(); releaseErr != nil {
			resultErr = errors.Join(resultErr, releaseErr)
			if result == nil {
				result = &Log{closed: true, closeErr: resultErr}
			}
			result.startupDebt, result.closeErr = work, resultErr
			result.terminalErr = resultErr
		}
	}()
	flags := os.O_RDWR
	if o.ReadOnly {
		flags = os.O_RDONLY
	}
	file, err = os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, receipt, err
	}
	captured, err := file.Stat()
	named, nameErr := os.Lstat(path)
	if err != nil || nameErr != nil || !captured.Mode().IsRegular() || !os.SameFile(captured, named) || captured.Size() < saved.Offset || captured.Size() > o.MaxBytes {
		return nil, receipt, errors.Join(ErrCorrupt, err, nameErr)
	}
	var header [frameHeaderSize]byte
	if _, err := file.ReadAt(header[:], 0); err != nil {
		return nil, receipt, errors.Join(ErrCorrupt, err)
	}
	length := int64(binary.BigEndian.Uint32(header[24:28]))
	if length > MaxPayloadSize || length+frameHeaderSize+frameMACSize > saved.Offset {
		return nil, receipt, ErrCorrupt
	}
	firstBytes := length + frameHeaderSize + frameMACSize
	first, partial, err := scanContext(ctx, io.NewSectionReader(file, 0, firstBytes), key, nil)
	if err != nil || partial || first.FormatVersion != boundFrameVersion || first.FileRevision != 1 || first.TrimmedThrough != 0 || first.Sequence != 0 || first.Baseline != o.Binding || saved.Sequence == 0 && saved != first || saved.Sequence > 0 && saved.Offset <= first.Offset {
		return nil, receipt, errors.Join(ErrCorrupt, err)
	}
	decoded := uint64(0)
	head, partial, err := scanFromContext(ctx, io.NewSectionReader(file, saved.Offset, captured.Size()-saved.Offset), key, saved, false, func(Record) error { decoded++; return nil })
	if err != nil || partial || head.Offset != captured.Size() {
		return nil, receipt, errors.Join(ErrCorrupt, err)
	}
	now, statErr := file.Stat()
	named, nameErr = os.Lstat(path)
	current, found, selectionErr := o.Publisher.Select(o.Authority)
	if statErr != nil || nameErr != nil || !os.SameFile(captured, now) || !os.SameFile(captured, named) || now.Size() != captured.Size() || !now.ModTime().Equal(captured.ModTime()) || selectionErr != nil || !found || current != selected || !o.Guard.CoversNativePath(path) || ctx.Err() != nil {
		return nil, receipt, errors.Join(ErrCorrupt, statErr, nameErr, selectionErr, ctx.Err())
	}
	if _, err := file.Seek(head.Offset, io.SeekStart); err != nil {
		return nil, receipt, err
	}
	writer := durabilityFor(file)
	if writer == nil {
		return nil, receipt, ErrCorrupt
	}
	result = &Log{readOnly: o.ReadOnly, file: file, durability: writer, path: path, key: append([]byte(nil), key...), epoch: head.Epoch, sequence: head.Sequence, offset: head.Offset, lastHash: head.Hash, formatVersion: boundFrameVersion, fileRevision: 1, baseline: o.Binding, maxBytes: o.MaxBytes, startupAuditPending: true, startupBudget: o.Budget, startupSaved: saved, startupGuard: o.Guard}
	return result, NativeStartupReceipt{Saved: saved, Head: head, DecodedRecords: decoded, HistoricalAuditPending: true}, nil
}

func (l *Log) SelectedStartupReady() bool { return l != nil && l.HealthError() == nil }

// ReplaySelectedDeltaContext authenticates the complete current suffix twice
// around a visitor, without traversing the selected historical prefix. It is
// usable while historical admission remains pending, solely for private root
// reduction. It grants no business/role/Seal readiness. The callback may read
// this Log but must not close it or retain its context/records.
func (l *Log) ReplaySelectedDeltaContext(ctx context.Context, budget *resourcebudget.Manager, visit func(context.Context, Record) error) (resultErr error) {
	if l == nil || budget == nil || visit == nil {
		return ErrCorrupt
	}
	work, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: FrozenReadRAMBytes})
	if err != nil {
		return err
	}
	opCtx, cancel := context.WithCancel(ctx)
	l.mu.Lock()
	if err := l.descriptorAvailabilityError(); err != nil || l.startupBudget != budget || l.startupSaved.Offset <= 0 || l.terminalErr != nil || l.freezeCancel != nil {
		failure := errors.Join(ErrCorrupt, err, l.terminalErr)
		if l.freezeCancel != nil {
			failure = ErrNativeFrozen
		}
		l.mu.Unlock()
		cancel()
		return errors.Join(failure, work.Release())
	}
	frozen := Head{Epoch: l.epoch, Sequence: l.sequence, Offset: l.offset, Hash: l.lastHash, FormatVersion: l.formatVersion, FileRevision: l.fileRevision, Baseline: l.baseline, TrimmedThrough: l.trimmedThrough}
	saved, file, key := l.startupSaved, l.file, l.key
	l.freezeCancel = cancel
	l.freezeWG.Add(1)
	l.mu.Unlock()
	defer l.freezeWG.Done()
	defer func() {
		cancel()
		l.mu.Lock()
		l.freezeCancel = nil
		l.mu.Unlock()
		if releaseErr := work.Release(); releaseErr != nil {
			resultErr = errors.Join(resultErr, releaseErr)
			l.mu.Lock()
			l.freezeDebt, l.terminalErr = work, errors.Join(l.terminalErr, releaseErr)
			l.mu.Unlock()
		}
	}()
	check := func() error {
		l.mu.Lock()
		defer l.mu.Unlock()
		return errors.Join(opCtx.Err(), l.descriptorAvailabilityError(), l.checkPreparedFile())
	}
	if err := check(); err != nil {
		return err
	}
	verify := func(callback func(Record) error) error {
		actual, partial, err := scanFromContext(opCtx, io.NewSectionReader(file, saved.Offset, frozen.Offset-saved.Offset), key, saved, false, callback)
		if err != nil {
			return err
		}
		if partial || actual != frozen {
			return ErrCorrupt
		}
		return check()
	}
	if err := verify(nil); err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			l.mu.Lock()
			l.terminalErr = errors.Join(l.terminalErr, err)
			l.mu.Unlock()
		}
		return err
	}
	callbackErr := verify(func(record Record) error { return visit(opCtx, record) })
	if err := opCtx.Err(); err != nil {
		return errors.Join(callbackErr, err)
	}
	return errors.Join(callbackErr, verify(nil))
}

// AuditSelectedStartup uses the same budget and actual frozen descriptor. Close
// cancels and joins its complete job. A canceled audit remains pending; a native
// integrity failure permanently retires admission. No old bytes are repaired.
func (l *Log) AuditSelectedStartup(ctx context.Context, budget *resourcebudget.Manager) error {
	return l.auditSelectedStartup(ctx, budget, nil)
}

// phase is a test seam reached after an actual authenticated historical frame.
func (l *Log) auditSelectedStartup(ctx context.Context, budget *resourcebudget.Manager, phase func(context.Context, Record)) error {
	if l == nil || budget == nil {
		return ErrCorrupt
	}
	l.mu.Lock()
	if l.startupAudit != nil {
		l.mu.Unlock()
		return ErrStartupAuditPending
	}
	if !l.startupAuditPending || l.startupBudget != budget || l.startupDebt != (resourcebudget.Permit{}) {
		l.mu.Unlock()
		return ErrCorrupt
	}
	saved := l.startupSaved
	l.mu.Unlock()
	err := l.WithFrozenHead(ctx, budget, func(ctx context.Context, head Head) error {
		l.mu.Lock()
		file, key := l.file, l.key
		l.mu.Unlock()
		matched := false
		actual, partial, scanErr := scanContext(ctx, io.NewSectionReader(file, 0, head.Offset), key, func(record Record) error {
			if phase != nil {
				phase(ctx, record)
			}
			if record.Sequence == saved.Sequence {
				matched = record.Epoch == saved.Epoch && record.Hash == saved.Hash
				if matched {
					var boundary [frameHeaderSize]byte
					if _, err := file.ReadAt(boundary[:], record.Offset); err != nil {
						return err
					}
					matched = record.Offset+frameHeaderSize+int64(binary.BigEndian.Uint32(boundary[24:28]))+frameMACSize == saved.Offset
				}
			}
			return nil
		})
		if scanErr != nil || partial || actual != head || !matched {
			failure := errors.Join(ErrCorrupt, scanErr)
			if !errors.Is(scanErr, context.Canceled) && !errors.Is(scanErr, context.DeadlineExceeded) {
				l.mu.Lock()
				l.terminalErr = errors.Join(l.terminalErr, failure)
				l.mu.Unlock()
			}
			return failure
		}
		return nil
	})
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := errors.Join(ctx.Err(), l.descriptorAvailabilityError(), l.terminalErr); err != nil {
		return err
	}
	l.startupAuditPending = false
	return nil
}
