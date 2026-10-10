package authenticatedstate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const maxPhysicalTerminalSlots = resourcebudget.MaxCompletionBatch

type physicalRootSlot struct {
	file                                 *os.File
	owner, alias                         *ownedfile.Owner
	window                               *PhysicalTerminalWindow
	prepared, closed, published, retired bool
	recovery                             bool
	pendingMove                          bool
	closeErr                             error
}

type physicalStoreOwner struct {
	receipt                  *physicalReceipt
	maxObservedTerminalCount int64
	debt                     int64
	allocatedEnd             int64
	slots                    [maxPhysicalTerminalSlots]*physicalRootSlot
	fee                      resourcebudget.Permit
	failure                  error
	windows                  int
	ramRetired               bool
	holdForRecovery          bool
	feeRetired               bool
}

// PhysicalTerminalWindow owns actual pack space and actual future root inodes.
// It grants no saved-root, native-cut or role authority.
type PhysicalTerminalWindow struct {
	store    *Store
	slots    [maxPhysicalTerminalSlots]*physicalRootSlot
	count    int
	closed   bool
	closeErr error
}

type physicalUpdateKey struct{}

type PhysicalUpdate struct {
	store    *Store
	slot     *physicalRootSlot
	pack     os.FileInfo
	finished bool
}

// EnablePhysicalTerminal retains a finite aggregate control fee. The actual
// allocations are made at request admission, never inferred from the fee.
func (s *Store) EnablePhysicalTerminal(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closing || s.readOnly || s.fault() != nil {
		return ErrRecovery
	}
	if s.physical != nil {
		return nil
	}
	if err := errors.Join(s.checkPack(), s.checkRoot()); err != nil {
		return err
	}
	// This owner outlives a caller's temporary RAM loan and owns physical Disk.
	// Preserve cancellation and source authority, but acquire its real fee.
	fee, err := s.budget.TryAcquireContext(resourcebudget.WithoutRAMWorkspace(ctx), resourcebudget.Background, resourcebudget.Cost{RAM: 32 << 10, Disk: (maxPhysicalTerminalSlots + 2) * 4096})
	if err != nil {
		return err
	}
	s.physical = &physicalStoreOwner{fee: fee, allocatedEnd: int64(s.physicalExtent) + 4096}
	s.publishPackLocked()
	// Hold the real aggregate fee before any target allocation. A failed
	// allocation can have allocated a prefix; preserve its owner and fault.
	if err := resourcebudget.AllocatePhysicalRange(s.pack, int64(s.physicalExtent), 4096); err != nil {
		s.physical.failure = err
		s.poison(err)
		return err
	}
	return nil
}

func (s *Store) allocatePhysicalLocked(extra int64) error {
	if err := errors.Join(s.checkPack(), s.checkRoot(), s.fault()); err != nil {
		s.poison(err)
		return err
	}
	if s.closed || s.closing || s.repack != nil {
		return ErrBusy
	}
	start := int64(s.physicalExtent)
	if extra <= 0 || s.physical.debt > s.ceiling-start || extra > s.ceiling-start-s.physical.debt {
		return ErrLimit
	}
	end := start + s.physical.debt + extra
	priorEnd := s.physical.allocatedEnd
	if end > s.physical.allocatedEnd {
		s.physical.allocatedEnd = end
	}
	if err := s.savePhysicalReceiptLocked(); err != nil {
		s.poison(err)
		return err
	}
	err := resourcebudget.AllocatePhysicalRange(s.pack, start, s.physical.debt+extra)
	if errors.Is(err, resourcebudget.ErrPhysicalAllocationNoWrite) {
		// Only the explicit read-only preflight permits restoring this boundary.
		// An attempted syscall or Sync failure still owns its pessimistic range.
		s.physical.allocatedEnd = priorEnd
		if restoreErr := s.savePhysicalReceiptLocked(); restoreErr != nil {
			err = errors.Join(err, restoreErr)
			s.physical.failure = err
			s.poison(err)
		}
		return err
	}
	if err == nil || errors.Is(err, resourcebudget.ErrPhysicalAllocationUncertain) {
		if end := start + s.physical.debt + extra; end > s.physical.allocatedEnd {
			s.physical.allocatedEnd = end
		}
	}
	if errors.Is(err, resourcebudget.ErrPhysicalAllocationUncertain) {
		s.physical.failure = err
		s.poison(err)
	}
	return err
}

func (s *Store) ReservePhysicalTerminals(ctx context.Context, count int) (*PhysicalTerminalWindow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.physical == nil || count < 1 || count > maxPhysicalTerminalSlots {
		return nil, ErrLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	free := 0
	for _, slot := range s.physical.slots {
		if slot == nil {
			free++
		}
	}
	if free < count {
		return nil, ErrLimit
	}
	// This bounded demand is measured from actual reservations, not a caller's
	// projection of future history. Remember it even when known capacity rejects
	// this preaccept attempt, so an independently selected NoDebt checkpoint may
	// subsequently reclaim space without granting any new write permission.
	demand := int64(maxPhysicalTerminalSlots - free + count)
	if demand > s.physical.maxObservedTerminalCount {
		s.physical.maxObservedTerminalCount = demand
	}
	if err := s.allocatePhysicalLocked(int64(count) * s.appendLimit); err != nil {
		return nil, err
	}
	w := &PhysicalTerminalWindow{store: s}
	s.physical.windows++
	var prepareErr error
	for i := range s.physical.slots {
		if w.count == count {
			break
		}
		if s.physical.slots[i] != nil {
			continue
		}
		f, owner, err := ownedfile.Create(s.slotPath(s.packSlot, fmt.Sprintf("terminal-root-%02d", i)), os.O_RDWR, 0600)
		if err != nil {
			prepareErr = err
			break
		}
		slot := &physicalRootSlot{file: f, owner: owner, window: w}
		s.physical.slots[i], w.slots[w.count] = slot, slot
		w.count++
		s.physical.debt += s.appendLimit
		if err := s.phase("physical-pool-created"); err != nil {
			prepareErr = err
			break
		}
		if err := s.savePhysicalReceiptLocked(); err != nil {
			prepareErr = err
			break
		}
		if err := s.phase("physical-pool-owned"); err != nil {
			prepareErr = err
			break
		}
		if err := resourcebudget.AllocatePhysicalRange(f, 0, cowRootBytes); err != nil {
			prepareErr = err
			break
		}
		if err := s.phase("physical-pool-allocated"); err != nil {
			prepareErr = err
			break
		}
	}
	prepareErr = errors.Join(prepareErr, syncDir(s.dir))
	if prepareErr != nil {
		return nil, errors.Join(prepareErr, w.closeLocked())
	}
	return w, nil
}

func (w *PhysicalTerminalWindow) closeLocked() error {
	if w.closed {
		return w.closeErr
	}
	w.closed = true
	var result error
	for _, slot := range w.slots[:w.count] {
		if slot.retired {
			continue
		}
		if slot.prepared {
			result = errors.Join(result, ErrRecovery)
			continue // accepted/uncertain children retain their real allocations.
		}
		result = errors.Join(result, w.store.retirePhysicalSlotLocked(slot, false))
	}
	if result != nil {
		w.store.poison(result)
	}
	w.closeErr = result
	if result == nil {
		w.store.physical.windows--
	}
	return result
}

func (w *PhysicalTerminalWindow) Close() error {
	if w == nil {
		return nil
	}
	w.store.mu.Lock()
	defer w.store.mu.Unlock()
	return w.closeLocked()
}

func (s *Store) retirePhysicalSlotLocked(slot *physicalRootSlot, published bool) error {
	if slot.retired {
		return nil
	}
	if !slot.closed {
		slot.closeErr = slot.file.Close()
		slot.closed = true
	}
	if slot.closeErr != nil {
		return errors.Join(ownedfile.ErrRetirement, slot.closeErr)
	}
	if !published && slot.alias != nil {
		if err := slot.alias.Retire(); err != nil {
			return err
		}
	}
	// The original spare is only an alias after publication. Removing it does
	// not refund published root bytes or make a reserve file a source authority.
	if published && slot.pendingMove {
		named, err := os.Lstat(s.slotPath(s.packSlot, "active.root"))
		if err != nil || !os.SameFile(named, slot.owner.Identity()) || !os.SameFile(named, s.rootInfo) {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
	}
	if slot.owner != nil && !(published && slot.pendingMove) {
		if err := slot.owner.Retire(); err != nil {
			return err
		}
	}
	if err := syncDir(s.dir); err != nil {
		return errors.Join(ownedfile.ErrRetirement, err)
	}
	s.physical.debt -= s.appendLimit
	slot.retired = true
	for i, candidate := range s.physical.slots {
		if candidate == slot {
			s.physical.slots[i] = nil
		}
	}
	return s.savePhysicalReceiptLocked()
}

// PreparePhysicalUpdate allocates an ordinary update ABOVE all terminal debt,
// or adopts one exact request-owned candidate. Called before native acceptance.
func (s *Store) PreparePhysicalUpdate(ctx context.Context, window *PhysicalTerminalWindow) (*PhysicalUpdate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.physical == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var slot *physicalRootSlot
	if window != nil {
		if window.store != s || window.closed {
			return nil, ErrRecovery
		}
		// A restored ordinary candidate may already own root.pending. Consume
		// that exact recorded inode before an unused future terminal slot;
		// numerical pool order supplies no alias/deletion authority.
		pendingInfo, pendingErr := os.Lstat(s.slotPath(s.packSlot, "root.pending"))
		if pendingErr != nil && !errors.Is(pendingErr, os.ErrNotExist) {
			return nil, pendingErr
		}
		if pendingErr == nil {
			for _, candidate := range window.slots[:window.count] {
				if candidate == nil || !candidate.recovery || candidate.prepared || candidate.retired {
					continue
				}
				info, err := candidate.file.Stat()
				if err != nil {
					return nil, err
				}
				if os.SameFile(pendingInfo, info) {
					slot = candidate
					break
				}
			}
			if slot == nil {
				return nil, ownedfile.ErrReplaced
			}
		} else {
			for _, candidate := range window.slots[:window.count] {
				if !candidate.prepared && !candidate.retired {
					slot = candidate
					break
				}
			}
		}
		if slot == nil {
			return nil, ErrLimit
		}
		if err := errors.Join(s.checkPack(), s.checkRoot(), slot.owner.VerifyFile(slot.file), s.fault()); err != nil {
			s.poison(err)
			return nil, err
		}
		if err := s.movePhysicalPendingLocked(slot); err != nil {
			return nil, err
		}

	} else {
		// Ordinary native acceptance also owes an actual COW publication. Its
		// candidate must have a MAC-recorded birth before any source byte is
		// accepted, just like terminal candidates. A bare root.pending cannot
		// be distinguished from a foreign inode after process death.
		index := -1
		for i, candidate := range s.physical.slots {
			if candidate == nil {
				index = i
				break
			}
		}
		if index < 0 {
			return nil, ErrLimit
		}
		if err := s.allocatePhysicalLocked(s.appendLimit); err != nil {
			return nil, err
		}
		f, owner, err := ownedfile.Create(s.slotPath(s.packSlot, fmt.Sprintf("terminal-root-%02d", index)), os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		slot = &physicalRootSlot{file: f, owner: owner}
		s.physical.slots[index] = slot
		s.physical.debt += s.appendLimit
		prepareErr := s.phase("physical-pool-created")
		if prepareErr == nil {
			prepareErr = s.savePhysicalReceiptLocked()
		}
		if prepareErr == nil {
			prepareErr = s.phase("physical-pool-owned")
		}
		if prepareErr == nil {
			prepareErr = resourcebudget.AllocatePhysicalRange(f, 0, cowRootBytes)
		}
		if prepareErr == nil {
			prepareErr = s.phase("physical-pool-allocated")
		}
		if prepareErr == nil {
			prepareErr = s.movePhysicalPendingLocked(slot)
		}
		if prepareErr != nil {
			return nil, errors.Join(prepareErr, s.retirePhysicalSlotLocked(slot, false))
		}
	}
	slot.prepared = true
	return &PhysicalUpdate{store: s, slot: slot, pack: s.info}, nil
}

func (u *PhysicalUpdate) Bind(ctx context.Context) context.Context {
	if u == nil {
		return ctx
	}
	return context.WithValue(ctx, physicalUpdateKey{}, u)
}

// Finish distinguishes definite pre-native refusal from a possibly accepted
// fact. No cancellation/Close or logical release guesses away uncertain debt.
func (u *PhysicalUpdate) Finish(accepted bool, operationErr error) error {
	if u == nil {
		return nil
	}
	s := u.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if u.finished {
		return ErrRecovery
	}
	u.finished = true
	if accepted && (operationErr != nil || !u.slot.published) {
		if !u.slot.closed {
			u.slot.closeErr = u.slot.file.Close()
			u.slot.closed = true
		}
		err := errors.Join(ErrRecovery, operationErr, u.slot.closeErr)
		s.poison(err)
		return err
	}
	err := s.retirePhysicalSlotLocked(u.slot, u.slot.published)
	if err != nil {
		s.poison(err)
	}
	return err
}

func physicalUpdate(ctx context.Context, s *Store) *PhysicalUpdate {
	u, _ := ctx.Value(physicalUpdateKey{}).(*PhysicalUpdate)
	if u != nil && u.store == s && !u.finished {
		return u
	}
	return nil
}

func (s *Store) physicalPending(ctx context.Context, path string) (*os.File, *ownedfile.Owner, error) {
	if u := physicalUpdate(ctx, s); u != nil {
		if !os.SameFile(u.pack, s.info) || u.slot.closed || !u.slot.prepared {
			return nil, nil, ErrRecovery
		}
		if err := u.slot.alias.VerifyFile(u.slot.file); err != nil {
			return nil, nil, err
		}
		return u.slot.file, u.slot.alias, nil
	}
	if s.physical != nil {
		return nil, nil, resourcebudget.ErrPhysicalAllocation
	}
	return ownedfile.Create(path, os.O_RDWR, 0600)
}

func recordPhysicalClose(ctx context.Context, s *Store, err error) {
	if u := physicalUpdate(ctx, s); u != nil {
		u.slot.closed, u.slot.closeErr = true, err
	}
}

func (s *Store) closePhysicalLocked() error {
	if s.physical == nil {
		return nil
	}
	result := s.physical.failure
	if s.repack != nil {
		s.physical.holdForRecovery = true
	}
	if s.physical.holdForRecovery {
		// The COW result has not entered the independent common selection. Join
		// the actual descriptors, but retain every name, extent and fee needed
		// to replay it after a process exit, including unused root targets.
		for _, slot := range s.physical.slots {
			if slot == nil || slot.retired {
				continue
			}
			if !slot.closed {
				slot.closeErr = slot.file.Close()
				slot.closed = true
			}
			result = errors.Join(result, slot.closeErr)
		}
		return errors.Join(ErrRecovery, result, s.closePhysicalReceiptFilesLocked())
	}
	for _, slot := range s.physical.slots {
		if slot == nil || slot.retired {
			continue
		}
		if slot.prepared || slot.recovery {
			if !slot.closed {
				slot.closeErr = slot.file.Close()
				slot.closed = true
			}
			result = errors.Join(result, ErrRecovery, slot.closeErr)
		} else {
			result = errors.Join(result, s.retirePhysicalSlotLocked(slot, false))
		}
	}
	// Published/uncertain inodes keep the aggregate persistent claim. A runtime
	// Close does not provide an unlink receipt for the current root or pack.
	if result == nil && s.physical.debt == 0 && s.physical.windows == 0 {
		result = s.retirePhysicalPreparationLocked()
		if result != nil {
			return result
		}
		if !s.physical.ramRetired {
			result = s.physical.fee.ReleasePart(resourcebudget.Cost{RAM: 32 << 10})
			if result == nil {
				s.physical.ramRetired = true
			}
		}
		if result == nil {
			result = s.releaseExitedPhysicalFeeLocked()
		}
	} else if result == nil {
		result = ErrRecovery
	}
	if result != nil {
		result = errors.Join(result, s.closePhysicalReceiptFilesLocked())
	}
	return result
}

// The current pack/root are already covered by the base Store reservation.
// Extra physical credit may exit only after all fixed preparation names have
// disappeared; opaque preservation names retain their cost and no authority to
// delete them is manufactured here.
func (s *Store) releaseExitedPhysicalFeeLocked() error {
	p := s.physical
	if p == nil || p.feeRetired {
		return nil
	}
	if p.receipt != nil || p.debt != 0 || p.windows != 0 || p.failure != nil || !p.ramRetired {
		return ErrRecovery
	}
	for i := 0; i < 4; i++ {
		if _, err := os.Lstat(filepath.Join(s.dir, fmt.Sprintf("terminal-unqualified-%d", i))); err == nil {
			return nil // opaque inodes still exist, retaining their aggregate fee
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for i := 0; i < maxPhysicalTerminalSlots; i++ {
		if _, err := os.Lstat(s.slotPath(s.packSlot, fmt.Sprintf("terminal-root-%02d", i))); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(ErrRecovery, err)
		}
	}
	if err := p.fee.Release(); err != nil {
		return err
	}
	p.feeRetired = true
	return nil
}

// HoldPhysicalPreparationForRecovery prevents Close from retiring a vector
// whose result has not entered the independently selected common root. It
// grants neither write permission nor a new selection; only the existing
// post-publication retirement lane can subsequently clear the hold.
func (s *Store) HoldPhysicalPreparationForRecovery(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed || s.closing || s.physical == nil || s.physical.receipt == nil {
		return ErrRecovery
	}
	s.physical.holdForRecovery = true
	if err := errors.Join(s.checkPack(), s.checkRoot(), s.checkPhysicalReceiptControlsLocked()); err != nil {
		s.physical.failure = errors.Join(s.physical.failure, err)
		return err
	}
	return nil
}

// RetirePhysicalPreparation joins only a captured, unused target tail. The
// caller must hold the actual native maintenance lane before moving a common
// selection; live terminal debt cannot be retired by a new caller generation.
func (s *Store) RetirePhysicalPreparation(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.retirePhysicalPreparationLocked()
}

func (s *Store) retirePhysicalPreparationLocked() error {
	if err := s.retirePhysicalPreparationKeepingControlsLocked(); err != nil {
		return err
	}
	return s.retirePhysicalReceiptLocked()
}

// RetirePhysicalPreparationKeepingControls joins unused pack/spare debt while
// retaining the actual allocated dual-bank inodes for post-common rollover.
// The caller owns the actual maintenance lane and completed NoDebt publication.
func (s *Store) RetirePhysicalPreparationKeepingControls(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.retirePhysicalPreparationKeepingControlsLocked()
}

func (s *Store) retirePhysicalPreparationKeepingControlsLocked() error {
	p := s.physical
	if p == nil {
		return nil
	}
	if s.closed {
		if s.closeErr == nil && p.failure == nil && p.ramRetired && p.debt == 0 && p.windows == 0 && p.receipt == nil {
			return nil
		}
		return errors.Join(ErrRecovery, s.closeErr, p.failure)
	}
	if p.failure != nil || p.debt != 0 || p.windows != 0 {
		return errors.Join(ErrRecovery, p.failure)
	}
	for _, slot := range p.slots {
		if slot != nil {
			return ErrRecovery
		}
	}
	if err := errors.Join(s.checkPack(), s.checkRoot(), s.fault()); err != nil {
		p.failure = err
		s.poison(err)
		return err
	}
	info, err := s.pack.Stat()
	if err == nil && p.allocatedEnd > info.Size() {
		err = resourcebudget.RetirePhysicalTail(s.pack, info.Size(), p.allocatedEnd)
	}
	if err != nil {
		p.failure = err
		s.poison(err)
		return err
	}
	p.allocatedEnd = info.Size()
	if err := s.savePhysicalReceiptLocked(); err != nil {
		return err
	}
	p.holdForRecovery = false
	return nil
}

func (s *Store) discardPhysicalFeeLocked() error {
	if s.physical == nil {
		return nil
	}
	if s.physical.debt != 0 || s.physical.windows != 0 || !s.physical.ramRetired {
		return ErrRecovery
	}
	if s.physical.feeRetired {
		return nil
	}
	if err := s.physical.fee.Release(); err != nil {
		return err
	}
	s.physical.feeRetired = true
	return nil
}
