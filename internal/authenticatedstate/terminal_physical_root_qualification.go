package authenticatedstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

// WithPhysicalRecoveryRoot reads only the exact root captured by a separately
// MAC-authenticated preparation receipt under an independent common selection.
// It does not promote Store.current or lend a target. The domain callback must
// read its bounded prefix scalar, authenticate that prefix and recordedNative
// against the actual selected native source, and derive the exact expected cut.
// A candidate's raw cut is a comparison value, never native authority.
func (s *Store) WithPhysicalRecoveryRoot(ctx context.Context, selection [32]byte, native PhysicalNativeReference, visit func(*Snapshot, [32]byte, PhysicalNativeReference) ([32]byte, error)) (cut [32]byte, found bool, resultErr error) {
	if s == nil || selection == ([32]byte{}) || native.File.Inode == 0 || native.Parent.Inode == 0 || visit == nil {
		return cut, false, ErrSealIneligible
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closing || s.readOnly || s.physical != nil {
		return cut, false, ErrRecovery
	}
	if err := errors.Join(ctx.Err(), s.checkPack(), s.checkRoot(), s.fault()); err != nil {
		return cut, false, err
	}
	var c physicalReceiptClaims
	valid, present := false, false
	for i := 0; i < 2; i++ {
		raw, _, err := readFixed(s.receiptPath(i), physicalReceiptBytes)
		if !errors.Is(err, os.ErrNotExist) {
			present = true
		}
		if err != nil {
			continue
		}
		candidate, err := decodePhysicalReceipt(raw, s.key)
		if err == nil && (!valid || candidate.Serial > c.Serial) {
			c = candidate
			valid = true
		}
	}
	if !valid {
		if present {
			return cut, false, ErrPhysicalReceiptRequiresNoDebt
		}
		return cut, false, nil
	}
	var pendingErr error
	c, pendingErr = s.qualifyPhysicalPublishedPendingLocked(c)
	if pendingErr != nil {
		return cut, false, pendingErr
	}
	if c.COWRoot == ([cowRootBytes]byte{}) || c.COWRootFile.Inode == 0 {
		return cut, false, ErrPhysicalReceiptRequiresNoDebt
	}
	pack, err := resourcebudget.CapturePhysicalIdentity(s.pack)
	if err != nil {
		return cut, false, err
	}
	parent, _, err := physicalPathIdentity(s.dir)
	if err != nil {
		return cut, false, err
	}
	rootFile, rootInfo, err := physicalPathIdentity(s.slotPath(s.packSlot, "active.root"))
	if err != nil {
		return cut, false, err
	}
	if c.Selection != selection || c.Scope != s.current.content.context || c.Pack != pack || c.Parent != parent || c.Native.File != native.File || c.Native.Parent != native.Parent || c.Native.Generation != native.Generation || c.Native.Sequence > native.Sequence || c.Native.Sequence == native.Sequence && c.Native.Hash != native.Hash || c.Native.EOF > native.EOF || c.NativeEnd < native.EOF || c.COWEnd < int64(s.physicalExtent) || c.COWEnd > s.ceiling || rootFile != c.COWRootFile || !os.SameFile(rootInfo, s.rootInfo) {
		return cut, false, ErrSealIneligible
	}
	raw, _, err := readFixed(s.slotPath(s.packSlot, "active.root"), cowRootBytes)
	if err != nil || !bytes.Equal(raw, c.COWRoot[:]) || sha256.Sum256(raw) != s.physicalRootDigest {
		return cut, false, errors.Join(ErrRecovery, err)
	}
	copy(cut[:], raw[8:40])
	root, err := decodeCOWRoot(raw, s.key, c.Scope, cut)
	if err != nil || root.content.id != s.current.content.id || root.revision < s.current.revision || root.content.extent != s.physicalExtent || c.COWEOF != int64(root.content.extent) {
		return cut, false, errors.Join(ErrSealIneligible, err)
	}
	pin, err := s.snapshotLocked(ctx)
	if err != nil {
		return cut, false, err
	}
	pin.current, pin.headerDigest = root.content, s.physicalRootDigest
	defer func() {
		if err := pin.Close(); err != nil {
			resultErr = errors.Join(resultErr, err)
			s.retirementErr = errors.Join(s.retirementErr, err)
			s.poison(err)
		}
	}()
	expected, err := visit(pin, cut, c.Native)
	if err != nil || expected != cut {
		return cut, false, errors.Join(ErrSealIneligible, err)
	}
	if err := errors.Join(s.checkPack(), s.checkRoot(), ctx.Err()); err != nil {
		return cut, false, err
	}
	return expected, true, nil
}
