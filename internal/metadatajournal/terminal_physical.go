package metadatajournal

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// PhysicalAppendBytes covers every legal format2 transaction including framing.
const PhysicalAppendBytes int64 = MaxPayloadSize + frameHeaderSize + frameMACSize

type physicalNativeOwner struct {
	parent          *os.File
	file, directory os.FileInfo
	end             int64
	failure         error
	enabled         bool
	hold            bool
	parentCloseDebt bool
	bound           *boundPhysicalOwner
}

// PhysicalNativeLoan is valid only inside the actual native owner's mutex.
// It lends the captured descriptor, never a pathname-derived candidate grant.
type PhysicalNativeLoan struct {
	mu     sync.Mutex
	log    *Log
	closed bool
}
type physicalAppendKey struct{}
type PhysicalAppend struct {
	log         *Log
	offset, end int64
	accepted    atomic.Bool
}

func (p *PhysicalAppend) Bind(ctx context.Context) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, physicalAppendKey{}, p)
}
func (p *PhysicalAppend) Accepted() bool { return p != nil && p.accepted.Load() }

func (l *Log) WithPhysicalNative(ctx context.Context, manager *resourcebudget.Manager, visit func(*PhysicalNativeLoan) error) error {
	return l.withPhysicalNative(ctx, manager, false, visit)
}

func (l *Log) WithPhysicalNativeRecovery(ctx context.Context, manager *resourcebudget.Manager, visit func(*PhysicalNativeLoan) error) error {
	return l.withPhysicalNative(ctx, manager, true, visit)
}

func (l *Log) withPhysicalNative(ctx context.Context, manager *resourcebudget.Manager, recovery bool, visit func(*PhysicalNativeLoan) error) (resultErr error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	check := l.availabilityError()
	if recovery {
		check = l.descriptorAvailabilityError()
		if l.startupGuard == nil || !l.startupGuard.CoversNativePath(l.path) {
			check = authenticatedstate.ErrSealIneligible
		}
	}
	if err := errors.Join(ctx.Err(), check); err != nil {
		return err
	}
	if visit == nil || manager == nil || l.startupBudget != manager || l.startupGuard == nil || l.readOnly || l.replica || l.boundReplication != nil || l.beforeDurable != nil || l.afterDurable != nil || l.formatVersion != boundFrameVersion || l.fileRevision != 1 || l.terminalErr != nil {
		return authenticatedstate.ErrSealIneligible
	}
	return l.withPhysicalNativeLoanLocked(ctx, visit)
}

func (l *Log) withPhysicalNativeLoanLocked(ctx context.Context, visit func(*PhysicalNativeLoan) error) (resultErr error) {
	if l.physical == nil {
		if err := l.checkPreparedFile(); err != nil {
			return err
		}
		parent, err := os.Open(filepath.Dir(l.path))
		if err != nil {
			return err
		}
		p := &physicalNativeOwner{parent: parent, end: l.offset}
		l.physical = p
		p.file, err = l.file.Stat()
		if err == nil {
			p.directory, err = parent.Stat()
		}
		if err == nil {
			err = l.checkPhysicalNativeLocked()
		}
		if err != nil {
			p.failure = err
			return err
		}
	}
	if err := l.checkPhysicalNativeLocked(); err != nil {
		return err
	}
	loan := &PhysicalNativeLoan{log: l}
	defer func() {
		loan.mu.Lock()
		loan.closed = true
		loan.mu.Unlock()
		resultErr = errors.Join(resultErr, ctx.Err(), l.checkPhysicalNativeLocked())
	}()
	err := visit(loan)
	if errors.Is(err, ownedfile.ErrRetirement) || errors.Is(err, ownedfile.ErrReplaced) || errors.Is(err, resourcebudget.ErrPhysicalAllocationUncertain) {
		l.physical.failure = err
		l.terminalErr = err
	}
	return errors.Join(err, l.checkPhysicalNativeLocked())
}
func (l *Log) checkPhysicalNativeLocked() error {
	p := l.physical
	if p == nil {
		return nil
	}
	if p.failure != nil {
		return p.failure
	}
	actual, err := l.file.Stat()
	named, ne := os.Lstat(l.path)
	parent, pe := p.parent.Stat()
	parentName, pne := os.Lstat(filepath.Dir(l.path))
	if err != nil || ne != nil || pe != nil || pne != nil || !actual.Mode().IsRegular() || named.Mode()&os.ModeSymlink != 0 || !os.SameFile(actual, p.file) || !os.SameFile(actual, named) || actual.Size() != l.offset || !parent.IsDir() || !os.SameFile(parent, p.directory) || !os.SameFile(parent, parentName) {
		p.failure = errors.Join(ownedfile.ErrReplaced, err, ne, pe, pne)
		l.terminalErr = p.failure
		return p.failure
	}
	return nil
}
func (q *PhysicalNativeLoan) Reference() (authenticatedstate.PhysicalNativeReference, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.referenceLocked()
}

// PreparedEnd reports this original captured writer's complete allocation
// range. Callers must retain it in their receipt even when a later terminal
// consumes debt and therefore needs a smaller future range. It grants neither
// append permission nor permission to adopt a range from a saved candidate.
func (q *PhysicalNativeLoan) PreparedEnd() (int64, error) {
	if q == nil {
		return 0, authenticatedstate.ErrClosed
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return 0, authenticatedstate.ErrClosed
	}
	if err := q.log.checkPhysicalNativeLocked(); err != nil {
		return 0, err
	}
	end := q.log.physical.end
	if err := resourcebudget.CheckPhysicalTail(q.log.file, q.log.offset, end); err != nil {
		return 0, err
	}
	return end, nil
}
func (q *PhysicalNativeLoan) referenceLocked() (authenticatedstate.PhysicalNativeReference, error) {
	if q == nil || q.closed {
		return authenticatedstate.PhysicalNativeReference{}, authenticatedstate.ErrClosed
	}
	l := q.log
	file, err := resourcebudget.CapturePhysicalIdentity(l.file)
	parent, pe := resourcebudget.CapturePhysicalIdentity(l.physical.parent)
	return authenticatedstate.PhysicalNativeReference{File: file, Parent: parent, Generation: l.epoch, Sequence: l.sequence, Hash: l.lastHash, EOF: l.offset}, errors.Join(err, pe)
}

// VerifyPrefix independently authenticates all receipt-bound native bytes on
// the actual descriptor. Recovery cannot use the receipt's head as authority.
func (q *PhysicalNativeLoan) AuthenticatePrefix(ctx context.Context, recorded authenticatedstate.PhysicalNativeReference) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.authenticatePrefixLocked(ctx, recorded)
}
func (q *PhysicalNativeLoan) authenticatePrefixLocked(ctx context.Context, recorded authenticatedstate.PhysicalNativeReference) error {
	actual, err := q.referenceLocked()
	if err != nil {
		return err
	}
	if recorded.File != actual.File || recorded.Parent != actual.Parent || recorded.Generation != actual.Generation || recorded.EOF < 0 || recorded.EOF > actual.EOF {
		return authenticatedstate.ErrSealIneligible
	}
	h, partial, err := scanContext(ctx, io.NewSectionReader(q.log.file, 0, recorded.EOF), q.log.key, nil)
	if err != nil || partial || h.Baseline != q.log.baseline || h.FileRevision != q.log.fileRevision || h.Epoch != recorded.Generation || h.Sequence != recorded.Sequence || h.Offset != recorded.EOF || h.Hash != recorded.Hash {
		return errors.Join(ErrCorrupt, err)
	}
	if err := q.log.checkPhysicalNativeLocked(); err != nil {
		return err
	}
	return nil
}

func (q *PhysicalNativeLoan) VerifyPrefix(ctx context.Context, recorded authenticatedstate.PhysicalNativeReference, end int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.authenticatePrefixLocked(ctx, recorded); err != nil {
		return err
	}
	if end < q.log.offset || end > q.log.maxBytes {
		return authenticatedstate.ErrSealIneligible
	}
	if err := resourcebudget.CheckPhysicalTail(q.log.file, q.log.offset, end); err != nil {
		return err
	}
	q.log.physical.end = end
	return nil
}

// Allocate persists a caller's authenticated receipt before reaching this API.
func (q *PhysicalNativeLoan) Allocate(end int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q == nil || q.closed {
		return authenticatedstate.ErrClosed
	}
	l := q.log
	if end < l.offset || end > l.maxBytes {
		return resourcebudget.ErrExhausted
	}
	if end <= l.physical.end {
		return l.checkPhysicalNativeLocked()
	}
	prior := l.physical.end
	l.physical.end = end // unknown allocation retains complete range
	if err := resourcebudget.AllocatePhysicalRange(l.file, prior, end-prior); err != nil {
		if errors.Is(err, resourcebudget.ErrPhysicalAllocationUncertain) {
			l.physical.failure = err
			l.terminalErr = err
		} else {
			l.physical.end = prior
		} // actual syscall proved no block/EOF change
		return err
	}
	return l.checkPhysicalNativeLocked()
}
func (q *PhysicalNativeLoan) Append() (*PhysicalAppend, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q == nil || q.closed {
		return nil, authenticatedstate.ErrClosed
	}
	l := q.log
	if l.boundPhysicalRecovery {
		return nil, authenticatedstate.ErrSealIneligible
	}
	if l.physical.end-l.offset < PhysicalAppendBytes {
		return nil, resourcebudget.ErrPhysicalAllocation
	}
	if err := resourcebudget.CheckPhysicalRange(l.file, l.offset, l.offset+PhysicalAppendBytes); err != nil {
		return nil, err
	}
	return &PhysicalAppend{log: l, offset: l.offset, end: l.physical.end}, nil
}
func (q *PhysicalNativeLoan) RetireUnused() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q == nil || q.closed {
		return authenticatedstate.ErrClosed
	}
	l := q.log
	if l.physical.end == l.offset {
		return l.checkPhysicalNativeLocked()
	}
	if err := resourcebudget.RetirePhysicalTail(l.file, l.offset, l.physical.end); err != nil {
		l.physical.failure = err
		l.terminalErr = err
		return err
	}
	l.physical.end = l.offset
	return l.checkPhysicalNativeLocked()
}
func (l *Log) validatePhysicalPreparedLocked(ctx context.Context, size int64) (*PhysicalAppend, error) {
	if l.physical == nil || !l.physical.enabled {
		return nil, nil
	}
	p, _ := ctx.Value(physicalAppendKey{}).(*PhysicalAppend)
	if p == nil || p.log != l || p.offset != l.offset || p.accepted.Load() || size <= 0 || size > PhysicalAppendBytes || p.end < l.offset+size {
		return nil, resourcebudget.ErrPhysicalAllocation
	}
	return p, l.checkPhysicalNativeLocked()
}
func (l *Log) closePhysicalNativeLocked() error {
	if l.physical == nil {
		return nil
	}
	p := l.physical
	if p.hold {
		return errors.Join(ownedfile.ErrRetirement, authenticatedstate.ErrRecovery)
	}
	if p.failure != nil {
		return errors.Join(ownedfile.ErrRetirement, p.failure)
	}
	if p.end > l.offset {
		if err := resourcebudget.RetirePhysicalTail(l.file, l.offset, p.end); err != nil {
			p.failure = errors.Join(ownedfile.ErrRetirement, err)
			return p.failure
		}
	}
	p.end = l.offset
	if err := ownedfile.CloseFile(p.parent); err != nil {
		p.parentCloseDebt = true
		p.failure = err
		return err
	}
	p.parent = nil
	return nil
}

// HeadAt authenticates the entire actual source and returns one exact prefix.
// The result is evidence for a receipt comparison, not external selection.
func (q *PhysicalNativeLoan) HeadAt(ctx context.Context, sequence uint64) (Head, Record, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q == nil || q.closed {
		return Head{}, Record{}, authenticatedstate.ErrClosed
	}
	l := q.log
	var last Record
	var end int64
	found := false
	h, partial, err := scanContext(ctx, io.NewSectionReader(l.file, 0, l.offset), l.key, func(r Record) error {
		if r.Sequence == sequence {
			last = r
			last.Ops = nil
			found = true
		}
		if r.Sequence == sequence+1 {
			end = r.Offset
		}
		return nil
	})
	if err != nil || partial || h.Epoch != l.epoch || h.Sequence != l.sequence || h.Offset != l.offset || h.Hash != l.lastHash || h.Baseline != l.baseline || !found {
		return Head{}, Record{}, errors.Join(ErrCorrupt, err)
	}
	if sequence == l.sequence {
		end = l.offset
	}
	h.Sequence, h.Hash, h.Offset = sequence, last.Hash, end
	return h, last, l.checkPhysicalNativeLocked()
}

func (q *PhysicalNativeLoan) Enable() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return authenticatedstate.ErrClosed
	}
	q.log.physical.enabled = true
	return q.log.checkPhysicalNativeLocked()
}

// HoldPhysicalForRecovery revokes IO and retains the authenticated target tail.
// It does not grant read, delete, append, or readiness permission.
func (l *Log) HoldPhysicalForRecovery() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.physical != nil {
		l.physical.hold = true
		l.terminalErr = errors.Join(l.terminalErr, authenticatedstate.ErrRecovery)
	}
}
func (l *Log) joinPhysicalFailureDescriptorsLocked(cause error) error {
	result := errors.Join(ownedfile.ErrRetirement, cause)
	if l.physical != nil && l.physical.parent != nil && !l.physical.parentCloseDebt {
		err := ownedfile.CloseFile(l.physical.parent)
		result = errors.Join(result, err)
		if err != nil {
			l.physical.parentCloseDebt = true
		} else {
			l.physical.parent = nil
		}
	}
	return errors.Join(result, l.closeCurrent())
}

// VerifyFutureCapacity checks actual unwritten extents needed by the restored
// count; an authenticated requested end alone never proves allocation success.
func (q *PhysicalNativeLoan) VerifyFutureCapacity(count int) error {
	if q == nil {
		return authenticatedstate.ErrClosed
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || count < 0 || count > resourcebudget.MaxCompletionBatch {
		return authenticatedstate.ErrClosed
	}
	if count == 0 {
		return q.log.checkPhysicalNativeLocked()
	}
	end := q.log.offset + int64(count)*PhysicalAppendBytes
	if end > q.log.physical.end {
		return resourcebudget.ErrPhysicalAllocation
	}
	return resourcebudget.CheckPhysicalRange(q.log.file, q.log.offset, end)
}
