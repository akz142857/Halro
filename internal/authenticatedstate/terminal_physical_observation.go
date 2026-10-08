package authenticatedstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// PhysicalTerminalWindowObservation describes only an original live allocation.
// It cannot select a root, reserve a target or grant native/role permission.
// Native sequence/EOF and the current root may advance within the same bank;
// selection, physical births and prepared ends identify the original bank.
type PhysicalTerminalWindowObservation struct {
	Selection, Scope               [32]byte
	Pack, Parent                   resourcebudget.PhysicalIdentity
	Controls                       [2]resourcebudget.PhysicalIdentity
	Native                         PhysicalNativeReference
	NativeEnd, COWEnd, AppendBytes int64
	UnusedRoots                    uint32
}

// PreparedObservation reads both authenticated controls and verifies each actual
// unused future inode and pack extent. The domain caller must still qualify its
// original native owner and separately subtract already received obligations.
func (w *PhysicalTerminalWindow) PreparedObservation(ctx context.Context) (out PhysicalTerminalWindowObservation, resultErr error) {
	if w == nil || w.store == nil {
		return out, ErrRecovery
	}
	s := w.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := errors.Join(ctx.Err(), s.checkPack(), s.checkRoot(), s.fault()); err != nil {
		return out, err
	}
	p := s.physical
	if w.closed || w.closeErr != nil || s.closed || s.closing || s.readOnly || p == nil || p.receipt == nil || p.windows < 1 || p.failure != nil || p.holdForRecovery || p.ramRetired || p.feeRetired || w.count < 1 || w.count > maxPhysicalTerminalSlots {
		return out, ErrRecovery
	}
	if _, err := p.fee.Cost(); err != nil {
		return out, err
	}
	if err := s.checkPhysicalReceiptControlsLocked(); err != nil {
		return out, err
	}
	r, c := p.receipt, p.receipt.claims
	var raw [physicalReceiptBytes]byte
	for i := range r.files {
		if n, err := r.files[i].ReadAt(raw[:], 0); err != nil || n != len(raw) {
			return out, errors.Join(err, io.ErrUnexpectedEOF)
		}
		bank, err := decodePhysicalReceipt(raw[:], s.key)
		if err != nil {
			if i != r.bank && c.Serial == 1 && raw == ([physicalReceiptBytes]byte{}) {
				continue
			}
			return out, err
		}
		if i == r.bank && bank != c || bank.Serial > c.Serial || bank.Serial == c.Serial && bank != c {
			return out, ErrCorrupt
		}
	}
	pack, err := resourcebudget.CapturePhysicalIdentity(s.pack)
	if err != nil {
		return out, err
	}
	parent, _, err := physicalPathIdentity(s.dir)
	if err != nil {
		return out, err
	}
	root, err := s.current.encode(s.key)
	if err != nil {
		return out, err
	}
	rootID, rootInfo, err := physicalPathIdentity(s.slotPath(s.packSlot, "active.root"))
	if err != nil {
		return out, err
	}
	if c.Selection == ([32]byte{}) || c.Scope != s.current.content.context || c.Pack != pack || c.Parent != parent || c.COWEOF != int64(s.physicalExtent) || c.COWEnd != p.allocatedEnd || c.COWEnd < c.COWEOF || c.COWEnd > s.ceiling || c.Debt != p.debt || c.PendingSlot != 0 || c.PendingRoot != ([cowRootBytes]byte{}) || c.COWRootFile != rootID || !os.SameFile(rootInfo, s.rootInfo) || !bytes.Equal(root, c.COWRoot[:]) || sha256.Sum256(root) != s.physicalRootDigest {
		return out, ErrRecovery
	}
	if _, err := os.Lstat(s.slotPath(s.packSlot, "root.pending")); !errors.Is(err, os.ErrNotExist) {
		return out, errors.Join(ownedfile.ErrReplaced, err)
	}
	for _, slot := range w.slots[:w.count] {
		if slot == nil || slot.window != w {
			return out, ErrRecovery
		}
		if slot.retired {
			continue
		}
		index := -1
		for i, actual := range p.slots {
			if actual == slot {
				index = i
				break
			}
		}
		if index < 0 || slot.closed || slot.closeErr != nil || slot.owner == nil || slot.file == nil {
			return out, ErrRecovery
		}
		if slot.prepared || slot.pendingMove {
			continue
		}
		if err := slot.owner.VerifyFile(slot.file); err != nil {
			return out, err
		}
		id, err := resourcebudget.CapturePhysicalIdentity(slot.file)
		if err != nil {
			return out, err
		}
		if id != c.Slots[index] {
			return out, ownedfile.ErrReplaced
		}
		if err := resourcebudget.CheckPhysicalRange(slot.file, 0, cowRootBytes); err != nil {
			return out, err
		}
		if err := resourcebudget.CheckPhysicalTail(slot.file, 0, cowRootBytes); err != nil {
			return out, err
		}
		out.UnusedRoots++
	}
	if int64(out.UnusedRoots) > p.debt/s.appendLimit || int64(out.UnusedRoots) > (c.COWEnd-c.COWEOF)/s.appendLimit {
		return PhysicalTerminalWindowObservation{}, ErrRecovery
	}
	if c.COWEnd > c.COWEOF {
		if err := resourcebudget.CheckPhysicalRange(s.pack, c.COWEOF, c.COWEnd); err != nil {
			return PhysicalTerminalWindowObservation{}, err
		}
	}
	if err := errors.Join(resourcebudget.CheckPhysicalTail(s.pack, c.COWEOF, c.COWEnd), s.checkPack(), s.checkRoot(), ctx.Err()); err != nil {
		return PhysicalTerminalWindowObservation{}, err
	}
	out.Selection, out.Scope, out.Pack, out.Parent, out.Controls = c.Selection, c.Scope, c.Pack, c.Parent, c.Controls
	out.Native, out.NativeEnd, out.COWEnd, out.AppendBytes = c.Native, c.NativeEnd, c.COWEnd, s.appendLimit
	return out, nil
}
