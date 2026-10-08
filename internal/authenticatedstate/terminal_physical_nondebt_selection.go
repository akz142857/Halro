package authenticatedstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
)

// EnablePhysicalTerminalAfterQualifiedNoDebt is a separate recovery boundary,
// never a relaxation of RestorePhysicalTerminalForRecovery. The real selected
// factory must independently authenticate the complete new common root at the
// full native head and exhaust both original recovery streams with NoDebt.
// An older selection may retire only when its exact receipt-bound physical root
// is this same independently selected root. The original native prefix, pool,
// control births and allocated ranges are fully checked before any retirement.
func (s *Store) EnablePhysicalTerminalAfterQualifiedNoDebt(ctx context.Context, selection [32]byte, native PhysicalNativeReference, expectedCut [32]byte, verifyNative func(PhysicalNativeReference, int64) error) (end int64, resultErr error) {
	if s == nil || selection == ([32]byte{}) || native.File.Inode == 0 || native.Parent.Inode == 0 || verifyNative == nil {
		return 0, ErrSealIneligible
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closing || s.readOnly || s.physical != nil || s.current.cut != expectedCut || s.current.content.extent != s.physicalExtent || s.rootDigest != s.physicalRootDigest {
		return 0, ErrRecovery
	}
	if err := errors.Join(ctx.Err(), s.checkPack(), s.checkRoot(), s.fault()); err != nil {
		return 0, err
	}
	var c physicalReceiptClaims
	valid := false
	for i := 0; i < 2; i++ {
		raw, _, err := readFixed(s.receiptPath(i), physicalReceiptBytes)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			continue
		}
		candidate, err := decodePhysicalReceipt(raw, s.key)
		if err == nil && (!valid || candidate.Serial > c.Serial) {
			c, valid = candidate, true
		}
	}
	if !valid || c.Selection == selection {
		// Existing first-run opaque preservation remains explicitly NoDebt.
		return s.enablePhysicalTerminalQualifiedLocked(ctx, selection, native, verifyNative)
	}
	var err error
	c, err = s.qualifyPhysicalPublishedPendingLocked(c)
	if err != nil {
		return 0, err
	}
	if c.COWRoot == ([cowRootBytes]byte{}) || c.COWRootFile.Inode == 0 {
		return 0, ErrSealIneligible
	}
	var transfer *physicalReceiptClaims
	if c.RepackTargetPack.Inode != 0 {
		qualified, err := s.qualifyPhysicalRepackNoDebtLocked(ctx, c, expectedCut)
		if err != nil {
			return 0, err
		}
		transfer = &qualified
	}
	comparison := c
	if transfer != nil {
		comparison = *transfer
	}
	raw, _, err := readFixed(s.slotPath(s.packSlot, "active.root"), cowRootBytes)
	if err != nil || !bytes.Equal(raw, comparison.COWRoot[:]) || sha256.Sum256(raw) != s.rootDigest {
		return 0, errors.Join(ErrSealIneligible, err)
	}
	root, err := decodeCOWRoot(raw, s.key, s.current.content.context, expectedCut)
	if err != nil || root != s.current || comparison.COWEOF != int64(root.content.extent) {
		return 0, errors.Join(ErrSealIneligible, err)
	}
	// This locked restore preserves the strict OLD selection and performs the
	// actual native, inode, extent, fee and finite slot checks without promotion
	// from candidate bytes: the root already equals independent Store.current.
	w, _, found, err := s.restorePhysicalTerminalClaimsLocked(ctx, c.Selection, native, expectedCut, verifyNative, &c, transfer)
	if err != nil || !found {
		return 0, errors.Join(ErrSealIneligible, err)
	}
	if err = w.closeLocked(); err != nil {
		return 0, err
	}
	if err = s.retirePhysicalPreparationKeepingControlsLocked(); err != nil {
		return 0, err
	}
	end = s.physical.receipt.claims.NativeEnd
	err = s.bindRetainedPhysicalSelectionLocked(selection, native)
	if err != nil {
		s.physical.failure = err
		s.poison(err)
	}
	return end, err
}
