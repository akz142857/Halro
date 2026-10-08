package authenticatedstate

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Only the MAC-recorded original slot may move to this single alternate name.
// The same birth remains owned before and after the no-replace rename; no
// hardlink escapes the ordinary inventory's exclusive-inode requirement.
func (s *Store) movePhysicalPendingLocked(slot *physicalRootSlot) error {
	path := s.slotPath(s.packSlot, "root.pending")
	if slot.pendingMove {
		if slot.alias == nil || slot.alias != slot.owner {
			return ErrRecovery
		}
		return slot.owner.VerifyFile(slot.file)
	}
	slot.pendingMove = true
	if err := s.savePhysicalReceiptLocked(); err != nil {
		return err
	}
	if err := s.phase("physical-pending-intent-synced"); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		err = slot.owner.RenameCaptured(path)
	} else if err == nil && slot.recovery {
		actual, statErr := slot.file.Stat()
		if statErr != nil || !os.SameFile(info, actual) {
			return errors.Join(ownedfile.ErrReplaced, statErr)
		}
		// Restore registered this exact pending name as the owner already.
		err = slot.owner.VerifyFile(slot.file)
	} else {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	if err != nil {
		return err
	}
	slot.alias = slot.owner
	if err := syncDir(s.dir); err != nil {
		return errors.Join(ownedfile.ErrRetirement, err)
	}
	return s.phase("physical-pending-renamed-synced")
}

// Qualify one of the two fixed names, never a missing inode or an arbitrary
// candidate. A durable intent may precede rename; no name guessing is needed.
func (s *Store) physicalReceiptSlotPathLocked(c physicalReceiptClaims, index int) (string, error) {
	fixed := s.slotPath(s.packSlot, fmt.Sprintf("terminal-root-%02d", index))
	if c.PendingSlot != uint8(index+1) {
		return fixed, nil
	}
	pending := s.slotPath(s.packSlot, "root.pending")
	_, fixedErr := os.Lstat(fixed)
	_, pendingErr := os.Lstat(pending)
	if fixedErr == nil && errors.Is(pendingErr, os.ErrNotExist) {
		return fixed, nil
	}
	if errors.Is(fixedErr, os.ErrNotExist) && pendingErr == nil {
		return pending, nil
	}
	return "", errors.Join(ownedfile.ErrReplaced, fixedErr, pendingErr)
}

// A completed root rename may precede receipt rollover. The original receipt
// explicitly recorded the intended root bytes and actual candidate birth;
// these only qualify owned physical storage. The domain must still authenticate
// its root scalar against the independently permitted actual native prefix.
func (s *Store) qualifyPhysicalPublishedPendingLocked(c physicalReceiptClaims) (physicalReceiptClaims, error) {
	if c.PendingSlot == 0 {
		return c, nil
	}
	if c.PendingSlot > maxPhysicalTerminalSlots {
		return c, ErrCorrupt
	}
	index := int(c.PendingSlot) - 1
	birth, _, err := physicalPathIdentity(s.slotPath(s.packSlot, "active.root"))
	if err != nil {
		return c, err
	}
	if birth != c.Slots[index] {
		return c, nil
	}
	if c.PendingRoot == ([cowRootBytes]byte{}) || c.Debt < s.appendLimit {
		return c, ErrRecovery
	}
	raw, _, err := readFixed(s.slotPath(s.packSlot, "active.root"), cowRootBytes)
	if err != nil || !bytes.Equal(raw, c.PendingRoot[:]) || sha256.Sum256(raw) != s.physicalRootDigest {
		return c, errors.Join(ErrRecovery, err)
	}
	var cut [32]byte
	copy(cut[:], raw[8:40])
	root, err := decodeCOWRoot(raw, s.key, c.Scope, cut)
	if err != nil || root.content.id != s.current.content.id || root.content.extent != s.physicalExtent || root.revision != s.physicalRevision || int64(root.content.extent) > c.COWEnd {
		return c, errors.Join(ErrRecovery, err)
	}
	fixed := s.slotPath(s.packSlot, fmt.Sprintf("terminal-root-%02d", index))
	if _, err := os.Lstat(fixed); !errors.Is(err, os.ErrNotExist) {
		return c, errors.Join(ownedfile.ErrReplaced, err)
	}
	if _, err := os.Lstat(s.slotPath(s.packSlot, "root.pending")); !errors.Is(err, os.ErrNotExist) {
		return c, errors.Join(ownedfile.ErrReplaced, err)
	}
	c.COWRoot, c.COWRootFile, c.COWEOF = c.PendingRoot, birth, int64(root.content.extent)
	c.Slots[index] = resourcebudget.PhysicalIdentity{}
	c.Debt -= s.appendLimit
	c.PendingSlot, c.PendingRoot = 0, [cowRootBytes]byte{}
	return c, nil
}

func (s *Store) checkFilledPhysicalRecoverySlotLocked(path string, c physicalReceiptClaims) error {
	raw, _, err := readFixed(path, cowRootBytes)
	if err != nil {
		return err
	}
	var comparison [32]byte
	copy(comparison[:], raw[8:40])
	r, err := decodeCOWRoot(raw, s.key, s.current.content.context, comparison)
	if err != nil || r.content.id != s.current.content.id || r.revision != s.physicalRevision+1 || r.content.extent < s.physicalExtent || int64(r.content.extent) > c.COWEnd || int64(r.content.extent) > s.info.Size() {
		return errors.Join(ErrRecovery, err)
	}
	// Candidate cut is a comparison, never a native/root authority.
	return nil
}

func (s *Store) checkPhysicalRecoveryPendingAliasLocked(c physicalReceiptClaims) error {
	path := s.slotPath(s.packSlot, "root.pending")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	id, actual, err := physicalPathIdentity(path)
	if err != nil || !os.SameFile(info, actual) || !info.Mode().IsRegular() || info.Size() != 0 && info.Size() != cowRootBytes || id == c.COWRootFile || c.PendingSlot == 0 || c.PendingSlot > maxPhysicalTerminalSlots {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	index := int(c.PendingSlot) - 1
	if id != c.Slots[index] {
		return ownedfile.ErrReplaced
	}
	if _, err := os.Lstat(s.slotPath(s.packSlot, fmt.Sprintf("terminal-root-%02d", index))); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	if info.Size() == cowRootBytes {
		return s.checkFilledPhysicalRecoverySlotLocked(path, c)
	}
	return nil
}
