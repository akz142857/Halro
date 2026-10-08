package authenticatedstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// ValidatePhysicalTerminalPreparation only verifies an already-owned, joined
// NoDebt preparation. Its caller independently proves current common selection
// and actual source permission. It never creates, rewrites, retires or lends a
// target, and does not turn a candidate receipt into source authority.
func (s *Store) ValidatePhysicalTerminalPreparation(ctx context.Context, selection [32]byte, native PhysicalNativeReference, verifyNative func(PhysicalNativeReference, int64) error) (bool, error) {
	if s == nil || selection == ([32]byte{}) || native.File.Inode == 0 || native.Parent.Inode == 0 || verifyNative == nil {
		return false, ErrSealIneligible
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := errors.Join(ctx.Err(), s.checkPack(), s.checkRoot(), s.fault()); err != nil {
		return false, err
	}
	if s.closed || s.closing || s.readOnly {
		return false, ErrRecovery
	}
	p := s.physical
	if p == nil {
		return false, nil
	}
	if p.receipt == nil || p.failure != nil || p.debt != 0 || p.windows != 0 || p.holdForRecovery || p.ramRetired || p.feeRetired {
		return true, errors.Join(ErrRecovery, p.failure)
	}
	if _, err := p.fee.Cost(); err != nil {
		return true, err
	}
	if err := s.checkPhysicalReceiptControlsLocked(); err != nil {
		return true, err
	}
	r, c := p.receipt, p.receipt.claims
	var raw [physicalReceiptBytes]byte
	for i := 0; i < 2; i++ {
		if n, err := r.files[i].ReadAt(raw[:], 0); err != nil || n != len(raw) {
			return true, errors.Join(err, io.ErrUnexpectedEOF)
		}
		bank, err := decodePhysicalReceipt(raw[:], s.key)
		if err != nil {
			// Only the freshly initialized inactive genesis bank can be blank.
			if i != r.bank && c.Serial == 1 && raw == ([physicalReceiptBytes]byte{}) {
				continue
			}
			return true, err
		}
		if i == r.bank && bank != c || bank.Serial > c.Serial || bank.Serial == c.Serial && bank != c {
			return true, ErrCorrupt
		}
	}
	pack, err := resourcebudget.CapturePhysicalIdentity(s.pack)
	if err != nil {
		return true, err
	}
	parent, _, err := physicalPathIdentity(s.dir)
	if err != nil {
		return true, err
	}
	rootFile, rootInfo, err := physicalPathIdentity(s.slotPath(s.packSlot, "active.root"))
	if err != nil {
		return true, err
	}
	root, err := s.current.encode(s.key)
	if err != nil || c.Selection != selection || c.Scope != s.current.content.context || c.Pack != pack || c.Parent != parent || c.Native != native || c.NativeEnd < native.EOF || c.COWEOF != int64(s.physicalExtent) || c.COWEnd != p.allocatedEnd || c.COWEnd < c.COWEOF || c.COWEnd > s.ceiling || c.COWRootFile != rootFile || !os.SameFile(rootInfo, s.rootInfo) || c.Debt != 0 || !bytes.Equal(root, c.COWRoot[:]) || sha256.Sum256(root) != s.physicalRootDigest {
		return true, errors.Join(ErrSealIneligible, err)
	}
	if c.PendingSlot != 0 || c.PendingRoot != ([cowRootBytes]byte{}) {
		return true, ErrRecovery
	}
	if _, err := os.Lstat(s.slotPath(s.packSlot, "root.pending")); !errors.Is(err, os.ErrNotExist) {
		return true, errors.Join(ownedfile.ErrReplaced, err)
	}
	for i, slot := range p.slots {
		if slot != nil || c.Slots[i].Inode != 0 {
			return true, ErrRecovery
		}
		if _, err := os.Lstat(s.slotPath(s.packSlot, fmt.Sprintf("terminal-root-%02d", i))); !errors.Is(err, os.ErrNotExist) {
			return true, errors.Join(ownedfile.ErrReplaced, err)
		}
	}
	if err := verifyNative(c.Native, c.NativeEnd); err != nil {
		return true, err
	}
	if c.COWEnd > int64(s.physicalExtent) {
		if err := resourcebudget.CheckPhysicalRange(s.pack, int64(s.physicalExtent), c.COWEnd); err != nil {
			return true, err
		}
	}
	return true, errors.Join(resourcebudget.CheckPhysicalTail(s.pack, int64(s.physicalExtent), c.COWEnd), s.checkPack(), s.checkRoot(), ctx.Err())
}
