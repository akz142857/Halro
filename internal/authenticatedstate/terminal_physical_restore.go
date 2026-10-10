package authenticatedstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Older receipts cannot lend a root or targets for pending recovery. The
// actual native caller may route them through its separate complete NoDebt
// proof, preserving the existing bounded spare-cleanup compatibility.
var ErrPhysicalReceiptRequiresNoDebt = errors.New("physical receipt lacks root recovery authority; complete NoDebt proof required")

// RestorePhysicalTerminalForRecovery never allocates a range, creates a pool or
// admits new work. The native caller independently selects the complete common
// seal, completes its actual full native audit, and supplies the exact native
// cut of this already-published root. Only the authenticated ownership receipt
// can name the newer physical root and its prepaid target inodes.
func (s *Store) RestorePhysicalTerminalForRecovery(ctx context.Context, selection [32]byte, native PhysicalNativeReference, expectedCut [32]byte, reserveNative func(PhysicalNativeReference, int64) error) (window *PhysicalTerminalWindow, end int64, found bool, resultErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restorePhysicalTerminalForRecoveryLocked(ctx, selection, native, expectedCut, reserveNative)
}

func (s *Store) restorePhysicalTerminalForRecoveryLocked(ctx context.Context, selection [32]byte, native PhysicalNativeReference, expectedCut [32]byte, reserveNative func(PhysicalNativeReference, int64) error) (window *PhysicalTerminalWindow, end int64, found bool, resultErr error) {
	return s.restorePhysicalTerminalClaimsLocked(ctx, selection, native, expectedCut, reserveNative, nil, nil)
}

// Overrides are private to the independently qualified NoDebt repack transfer.
// Public restore always consumes the original MAC claims unchanged.
func (s *Store) restorePhysicalTerminalClaimsLocked(ctx context.Context, selection [32]byte, native PhysicalNativeReference, expectedCut [32]byte, reserveNative func(PhysicalNativeReference, int64) error, from, to *physicalReceiptClaims) (window *PhysicalTerminalWindow, end int64, found bool, resultErr error) {
	if s.closed || s.closing || s.readOnly || s.physical != nil || selection == ([32]byte{}) || reserveNative == nil {
		return nil, 0, false, ErrRecovery
	}
	if err := errors.Join(ctx.Err(), s.checkPack(), s.checkRoot(), s.fault()); err != nil {
		return nil, 0, false, err
	}
	if err := s.checkUnqualifiedPhysicalSlotsLocked(); err != nil {
		return nil, 0, false, err
	}
	r := &physicalReceipt{bank: 1}
	valid, present := false, false
	for i := 0; i < 2; i++ {
		raw, _, readErr := readFixed(s.receiptPath(i), physicalReceiptBytes)
		if !errors.Is(readErr, os.ErrNotExist) {
			present = true
		}
		if readErr != nil {
			continue
		}
		claims, err := decodePhysicalReceipt(raw, s.key)
		if err == nil && (!valid || claims.Serial > r.claims.Serial) {
			r.claims, r.bank, valid = claims, i, true
		}
	}
	if !valid {
		for i := 0; i < maxPhysicalTerminalSlots; i++ {
			if _, err := os.Lstat(s.slotPath(s.packSlot, fmt.Sprintf("terminal-root-%02d", i))); !errors.Is(err, os.ErrNotExist) {
				present = true
			}
		}
		if _, err := os.Lstat(s.slotPath(s.packSlot, "root.pending")); !errors.Is(err, os.ErrNotExist) {
			present = true
		}
		if present {
			return nil, 0, false, ErrPhysicalReceiptRequiresNoDebt
		}
		if err := resourcebudget.CheckPhysicalTail(s.pack, int64(s.physicalExtent), int64(s.physicalExtent)); err != nil {
			return nil, 0, false, err
		}
		s.physicalRecoveryPending = false
		return nil, 0, false, nil
	}
	if to != nil {
		if from == nil || r.claims != *from {
			return nil, 0, false, ErrSealIneligible
		}
		r.claims = *to
	}
	c, publishedErr := s.qualifyPhysicalPublishedPendingLocked(r.claims)
	if publishedErr != nil {
		return nil, 0, false, publishedErr
	}
	r.claims = c
	// A different common selection is a pure eligibility rejection. It lends
	// no target and creates no owner. Only the separate fully qualified NoDebt
	// boundary may subsequently retire an exact root from that older selection.
	if c.Selection != selection {
		return nil, 0, false, ErrSealIneligible
	}
	// Older receipt formats may retire known spares only through the existing
	// NoDebt qualification. They cannot authorize a newer root for recovery.
	if c.COWRoot == ([cowRootBytes]byte{}) || c.COWRootFile.Inode == 0 {
		return nil, 0, false, ErrPhysicalReceiptRequiresNoDebt
	}
	// A crash before a spare's receipt/allocation completes can leave a fixed
	// trace without a lendable target. Only the separate native NoDebt gate
	// may enter the existing bounded opaque preservation/cleanup protocol.
	// Signed identity mismatches still follow the sticky rejection below.
	for i, expected := range c.Slots {
		path, pathErr := s.physicalReceiptSlotPathLocked(c, i)
		if pathErr != nil {
			return nil, 0, false, pathErr
		}
		if expected.Inode == 0 {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				return nil, 0, false, ErrPhysicalReceiptRequiresNoDebt
			}
			continue
		}
		id, info, err := physicalPathIdentity(path)
		if err != nil || id != expected || !info.Mode().IsRegular() || info.Size() != 0 && info.Size() != cowRootBytes {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, 0, false, err
		}
		rangeErr := resourcebudget.CheckPhysicalRange(f, 0, cowRootBytes)
		if err := ownedfile.CloseFile(f); err != nil {
			return nil, 0, false, err
		}
		if rangeErr != nil {
			return nil, 0, false, ErrPhysicalReceiptRequiresNoDebt
		}
	}
	fee, err := s.budget.TryAcquireContext(resourcebudget.WithoutRAMWorkspace(ctx), resourcebudget.Background, resourcebudget.Cost{RAM: 32 << 10, Disk: (5*(maxPhysicalTerminalSlots+2) + 5) * 4096})
	if err != nil {
		return nil, 0, false, err
	}
	s.physical = &physicalStoreOwner{fee: fee, receipt: r, allocatedEnd: c.COWEnd}
	s.publishPackLocked()
	defer func() {
		if resultErr != nil {
			s.physical.failure = resultErr
			s.poison(resultErr)
		}
	}()
	pack, err := resourcebudget.CapturePhysicalIdentity(s.pack)
	if err != nil {
		return nil, 0, false, err
	}
	parent, _, err := physicalPathIdentity(s.dir)
	if err != nil {
		return nil, 0, false, err
	}
	if c.Selection != selection || c.Scope != s.current.content.context || c.Pack != pack || c.Parent != parent || c.Native.File != native.File || c.Native.Parent != native.Parent || c.Native.Generation != native.Generation || c.Native.Sequence > native.Sequence || c.Native.Sequence == native.Sequence && c.Native.Hash != native.Hash || c.Native.EOF > native.EOF || c.NativeEnd < native.EOF || c.COWEnd < int64(s.physicalExtent) || c.COWEnd > s.ceiling {
		return nil, 0, false, ErrSealIneligible
	}
	if err := reserveNative(c.Native, c.NativeEnd); err != nil {
		return nil, 0, false, err
	}
	if s.info.Size() > c.COWEnd {
		return nil, 0, false, ErrRecovery
	}
	if err := s.checkPhysicalRecoveryPendingAliasLocked(c); err != nil {
		return nil, 0, false, err
	}
	rootFile, rootInfo, err := physicalPathIdentity(s.slotPath(s.packSlot, "active.root"))
	if err != nil || rootFile != c.COWRootFile || !os.SameFile(rootInfo, s.rootInfo) {
		return nil, 0, false, errors.Join(ownedfile.ErrReplaced, err)
	}
	raw, _, err := readFixed(s.slotPath(s.packSlot, "active.root"), cowRootBytes)
	if err != nil || !bytes.Equal(raw, c.COWRoot[:]) || sha256.Sum256(raw) != s.physicalRootDigest {
		return nil, 0, false, errors.Join(ErrRecovery, err)
	}
	root, err := decodeCOWRoot(c.COWRoot[:], s.key, c.Scope, expectedCut)
	if err != nil || root.content.id != s.current.content.id || root.revision < s.current.revision || root.content.extent != s.physicalExtent || c.COWEOF != int64(root.content.extent) {
		return nil, 0, false, errors.Join(ErrSealIneligible, err)
	}
	if err := resourcebudget.CheckPhysicalTail(s.pack, int64(s.physicalExtent), c.COWEnd); err != nil {
		return nil, 0, false, err
	}
	for i := 0; i < 2; i++ {
		id, info, err := physicalPathIdentity(s.receiptPath(i))
		if err != nil || id != c.Controls[i] || !info.Mode().IsRegular() || info.Size() != physicalReceiptBytes {
			return nil, 0, false, errors.Join(ownedfile.ErrReplaced, err)
		}
		r.files[i], err = os.OpenFile(s.receiptPath(i), os.O_RDWR, 0)
		if err != nil {
			return nil, 0, false, err
		}
		r.infos[i] = info
		actual, err := resourcebudget.CapturePhysicalIdentity(r.files[i])
		if err != nil || actual != c.Controls[i] {
			return nil, 0, false, errors.Join(ownedfile.ErrReplaced, err)
		}
		if err := resourcebudget.CheckPhysicalRange(r.files[i], 0, physicalReceiptBytes); err != nil {
			return nil, 0, false, err
		}
		if err := resourcebudget.CheckPhysicalTail(r.files[i], physicalReceiptBytes, physicalReceiptBytes); err != nil {
			return nil, 0, false, err
		}
	}
	w := &PhysicalTerminalWindow{store: s}
	for i, expected := range c.Slots {
		path, pathErr := s.physicalReceiptSlotPathLocked(c, i)
		if pathErr != nil {
			return nil, 0, false, pathErr
		}
		if expected.Inode == 0 {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				return nil, 0, false, errors.Join(ErrRecovery, err)
			}
			continue
		}
		id, info, err := physicalPathIdentity(path)
		if err != nil || id != expected || expected == c.COWRootFile || !info.Mode().IsRegular() || info.Size() != 0 && info.Size() != cowRootBytes {
			return nil, 0, false, errors.Join(ownedfile.ErrReplaced, err)
		}
		if info.Size() == cowRootBytes {
			if err := s.checkFilledPhysicalRecoverySlotLocked(path, c); err != nil {
				return nil, 0, false, err
			}
		}
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return nil, 0, false, err
		}
		slot := &physicalRootSlot{file: file, window: w, prepared: true, recovery: true}
		if path == s.slotPath(s.packSlot, "root.pending") {
			slot.pendingMove = true
		}
		s.physical.slots[i] = slot // Close must join every opened FD on any error.
		actual, err := resourcebudget.CapturePhysicalIdentity(file)
		if err != nil || actual != expected {
			return nil, 0, false, errors.Join(ownedfile.ErrReplaced, err)
		}
		slot.owner, err = ownedfile.RestoreAuthenticated(path, file, info, s.dirInfo)
		if err != nil {
			return nil, 0, false, err
		}
		if slot.pendingMove {
			slot.alias = slot.owner
		}
		if err := resourcebudget.CheckPhysicalTail(file, info.Size(), 4096); err != nil {
			return nil, 0, false, err
		}
		if err := resourcebudget.CheckPhysicalRange(file, 0, cowRootBytes); err != nil {
			return nil, 0, false, err
		}
		w.slots[w.count] = slot
		w.count++
	}
	if c.Debt != int64(w.count)*s.appendLimit || c.COWEnd-int64(s.physicalExtent) < c.Debt {
		return nil, 0, false, ErrRecovery
	}
	if c.Debt > 0 {
		if err := resourcebudget.CheckPhysicalRange(s.pack, int64(s.physicalExtent), int64(s.physicalExtent)+c.Debt); err != nil {
			return nil, 0, false, err
		}
	}
	for _, slot := range w.slots[:w.count] {
		slot.prepared = false
	}
	s.current, s.rootDigest = root, s.physicalRootDigest
	s.physicalRecoveryPending = false
	s.physical.debt, s.physical.windows = c.Debt, 1
	return w, c.NativeEnd, true, nil
}

func (w *PhysicalTerminalWindow) Count() int {
	if w == nil {
		return 0
	}
	return w.count
}
