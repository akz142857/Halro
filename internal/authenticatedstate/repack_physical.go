package authenticatedstate

import (
	"bytes"
	"errors"
	"os"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

func clearPhysicalRepack(c *physicalReceiptClaims) {
	c.RepackSourceSlot = 0
	c.RepackTargetPack = resourcebudget.PhysicalIdentity{}
	c.RepackTargetRootFile = resourcebudget.PhysicalIdentity{}
	c.RepackTargetRoot = [cowRootBytes]byte{}
	c.RepackTargetEOF = 0
}

func (c *StoreRepack) preparePhysicalReceiptLocked(file *os.File, slot uint8, next cowRoot, raw []byte, rootInfo os.FileInfo) error {
	s, p := c.store, c.store.physical
	if p == nil {
		return nil
	}
	if !c.old.physicalReceipt || p.failure != nil || p.debt != 0 || p.windows != 0 || p.receipt == nil || p.receipt.claims != c.old.physicalClaims {
		return ErrRecovery
	}
	for _, target := range p.slots {
		if target != nil {
			return ErrRecovery
		}
	}
	if err := s.checkPhysicalReceiptControlsLocked(); err != nil {
		return err
	}
	pack, err := resourcebudget.CapturePhysicalIdentity(file)
	if err != nil {
		return err
	}
	rootFile, actualRoot, err := physicalPathIdentity(s.slotPath(slot, "active.root"))
	if err != nil || !os.SameFile(rootInfo, actualRoot) || len(raw) != cowRootBytes || next.content.extent > uint64(s.ceiling) {
		return errors.Join(ErrRecovery, err)
	}
	r := &p.receipt.claims
	r.RepackSourceSlot, r.RepackTargetPack, r.RepackTargetRootFile = s.packSlot, pack, rootFile
	copy(r.RepackTargetRoot[:], raw)
	r.RepackTargetEOF = int64(next.content.extent)
	c.physicalIntent = true
	if err := s.savePhysicalReceiptLocked(); err != nil {
		return err
	}
	return s.phase("repack-physical-copy-qualified")
}

func (c *StoreRepack) rollbackPhysicalReceiptLocked() error {
	if !c.physicalIntent {
		return nil
	}
	p := c.store.physical
	if p == nil || p.receipt == nil || !c.old.physicalReceipt {
		return ErrRecovery
	}
	serial := p.receipt.claims.Serial
	p.receipt.claims = c.old.physicalClaims
	// Never restore the old serial: the qualified To bank must be superseded
	// durably before retiring its actual captured candidate.
	p.receipt.claims.Serial = serial
	if err := c.store.savePhysicalReceiptLocked(); err != nil {
		return err
	}
	c.physicalIntent = false
	return nil
}

// Called only after StoreRepack.finish authenticated the actual whole common
// publisher selecting this exact copied root, before the captured old FD exits.
// No logical equality or scanned candidate can transfer a preparation owner.
func (c *StoreRepack) commitPhysicalReceiptLocked() error {
	s := c.store
	p := s.physical
	if p == nil {
		return nil
	}
	if p.failure != nil || p.debt != 0 || p.windows != 0 || p.receipt == nil || !c.old.physicalReceipt {
		return ErrRecovery
	}
	for _, slot := range p.slots {
		if slot != nil {
			return ErrRecovery
		}
	}
	oldPack, err := resourcebudget.CapturePhysicalIdentity(c.old.file)
	oldClaims := p.receipt.claims
	clearPhysicalRepack(&oldClaims)
	oldClaims.Serial = c.old.physicalClaims.Serial
	if err != nil || oldPack != c.old.physicalClaims.Pack || oldClaims != c.old.physicalClaims || !c.physicalIntent {
		return errors.Join(ErrRecovery, err)
	}
	oldRoot, err := c.old.root.encode(s.key)
	if err != nil || !bytes.Equal(oldRoot, c.old.physicalClaims.COWRoot[:]) {
		return errors.Join(ErrRecovery, err)
	}
	if err := s.checkPhysicalReceiptControlsLocked(); err != nil {
		return err
	}
	if err := errors.Join(s.checkPack(), s.checkRoot(), s.fault()); err != nil {
		return err
	}
	newPack, err := resourcebudget.CapturePhysicalIdentity(s.pack)
	if err != nil {
		return err
	}
	newRoot, _, err := physicalPathIdentity(s.slotPath(s.packSlot, "active.root"))
	newBytes, encodeErr := s.current.encode(s.key)
	if err != nil || encodeErr != nil || p.receipt.claims.RepackTargetPack != newPack || p.receipt.claims.RepackTargetRootFile != newRoot || !bytes.Equal(newBytes, p.receipt.claims.RepackTargetRoot[:]) || p.receipt.claims.RepackTargetEOF != int64(s.physicalExtent) {
		return errors.Join(ErrRecovery, err, encodeErr)
	}
	parent, _, err := physicalPathIdentity(s.dir)
	if err != nil || parent != c.old.physicalClaims.Parent {
		return errors.Join(ErrRecovery, err)
	}
	p.receipt.claims.Pack, p.receipt.claims.Parent = newPack, parent
	clearPhysicalRepack(&p.receipt.claims)
	p.allocatedEnd = int64(s.physicalExtent)
	if err := s.savePhysicalReceiptLocked(); err != nil {
		return err
	}
	return s.phase("repack-physical-transfer-durable")
}
