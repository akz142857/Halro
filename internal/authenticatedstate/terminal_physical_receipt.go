package authenticatedstate

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const physicalReceiptBytes = 4096

// PhysicalNativeReference is supplied by the actual selected native owner,
// after complete authenticated startup recovery. It never comes from a file
// header being considered for physical preparation.
type PhysicalNativeReference struct {
	File, Parent         resourcebudget.PhysicalIdentity
	Generation, Sequence uint64
	Hash                 [32]byte
	EOF                  int64
}

type physicalReceiptClaims struct {
	Serial                          uint64
	Selection, Scope                [32]byte
	Native                          PhysicalNativeReference
	NativeEnd, COWEOF, COWEnd, Debt int64
	Pack, Parent                    resourcebudget.PhysicalIdentity
	Controls                        [2]resourcebudget.PhysicalIdentity
	Slots                           [maxPhysicalTerminalSlots]resourcebudget.PhysicalIdentity
	COWRoot                         [cowRootBytes]byte
	COWRootFile                     resourcebudget.PhysicalIdentity
	// A finite MAC-qualified copied successor. It never selects its own
	// contents; only a complete independent common root plus native NoDebt
	// may consume this transfer after a process exit.
	RepackSourceSlot     uint8
	RepackTargetPack     resourcebudget.PhysicalIdentity
	RepackTargetRootFile resourcebudget.PhysicalIdentity
	RepackTargetRoot     [cowRootBytes]byte
	RepackTargetEOF      int64
	PendingSlot          uint8
	PendingRoot          [cowRootBytes]byte
}

type physicalReceipt struct {
	claims   physicalReceiptClaims
	files    [2]*os.File
	infos    [2]os.FileInfo
	owners   [2]*ownedfile.Owner
	bank     int
	closeErr error
}

func (s *Store) receiptPath(bank int) string {
	return filepath.Join(s.dir, fmt.Sprintf("terminal.physical.%d", bank))
}

func physicalPathIdentity(path string) (resourcebudget.PhysicalIdentity, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return resourcebudget.PhysicalIdentity{}, nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return resourcebudget.PhysicalIdentity{}, nil, err
	}
	actual, statErr := f.Stat()
	id, identityErr := resourcebudget.CapturePhysicalIdentity(f)
	closeErr := ownedfile.CloseFile(f)
	if statErr != nil || !os.SameFile(info, actual) {
		return resourcebudget.PhysicalIdentity{}, nil, errors.Join(ownedfile.ErrReplaced, statErr, identityErr, closeErr)
	}
	return id, actual, errors.Join(identityErr, closeErr)
}

func encodePhysicalReceipt(c physicalReceiptClaims, key []byte) ([]byte, error) {
	var body bytes.Buffer
	if err := binary.Write(&body, binary.BigEndian, c); err != nil {
		return nil, err
	}
	if body.Len()+8+sha256.Size > physicalReceiptBytes {
		return nil, ErrLimit
	}
	raw := make([]byte, physicalReceiptBytes)
	copy(raw, "HLRPHY01")
	copy(raw[8:], body.Bytes())
	mac := hmac.New(sha256.New, key)
	mac.Write(raw[:len(raw)-sha256.Size])
	copy(raw[len(raw)-sha256.Size:], mac.Sum(nil))
	return raw, nil
}

func decodePhysicalReceipt(raw, key []byte) (physicalReceiptClaims, error) {
	var c physicalReceiptClaims
	if len(raw) != physicalReceiptBytes || string(raw[:8]) != "HLRPHY01" {
		return c, ErrCorrupt
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(raw[:len(raw)-sha256.Size])
	if !hmac.Equal(mac.Sum(nil), raw[len(raw)-sha256.Size:]) {
		return c, ErrCorrupt
	}
	if err := binary.Read(bytes.NewReader(raw[8:]), binary.BigEndian, &c); err != nil {
		return c, err
	}
	if c.PendingSlot > maxPhysicalTerminalSlots || c.Serial == 0 || c.Selection == ([32]byte{}) || c.Debt < 0 || c.Debt > 1<<50 || c.NativeEnd < c.Native.EOF || c.NativeEnd > 1<<50 || c.COWEnd < c.COWEOF || c.COWEnd > 1<<50 {
		return c, ErrCorrupt
	}
	return c, nil
}

func (s *Store) savePhysicalReceiptLocked() error {
	p := s.physical
	if p == nil || p.receipt == nil {
		return nil
	}
	r := p.receipt
	if r.claims.Serial == ^uint64(0) {
		return ErrLimit
	}
	// Only the actual already-published current root may enter the receipt.
	// A scanned newer header never becomes independent selection authority.
	rootBytes, err := s.current.encode(s.key)
	if err != nil || sha256.Sum256(rootBytes) != s.physicalRootDigest {
		return errors.Join(ErrRecovery, err)
	}
	copy(r.claims.COWRoot[:], rootBytes)
	rootFile, rootInfo, err := physicalPathIdentity(s.slotPath(s.packSlot, "active.root"))
	if err != nil || !os.SameFile(rootInfo, s.rootInfo) || rootInfo.Size() != cowRootBytes {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	r.claims.COWRootFile = rootFile
	r.claims.Serial++
	r.claims.COWEOF, r.claims.COWEnd, r.claims.Debt = int64(s.physicalExtent), p.allocatedEnd, p.debt
	r.claims.PendingSlot = 0
	for i, slot := range p.slots {
		if slot == nil {
			r.claims.Slots[i] = resourcebudget.PhysicalIdentity{}
			continue
		}
		id, err := resourcebudget.CapturePhysicalIdentity(slot.file)
		if err != nil {
			return err
		}
		r.claims.Slots[i] = id
		if slot.pendingMove {
			if r.claims.PendingSlot != 0 {
				return ErrRecovery
			}
			r.claims.PendingSlot = uint8(i + 1)
		}
	}
	if r.claims.PendingSlot == 0 {
		r.claims.PendingRoot = [cowRootBytes]byte{}
	}
	bank := 1 - r.bank
	id, _, err := physicalPathIdentity(s.receiptPath(bank))
	if err != nil || id != r.claims.Controls[bank] {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	actual, err := resourcebudget.CapturePhysicalIdentity(r.files[bank])
	if err != nil || actual != id {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	raw, err := encodePhysicalReceipt(r.claims, s.key)
	if err != nil {
		return err
	}
	n, err := r.files[bank].WriteAt(raw, 0)
	if err != nil || n != len(raw) {
		return errors.Join(ownedfile.ErrRetirement, err, io.ErrShortWrite)
	}
	if err := r.files[bank].Sync(); err != nil {
		return errors.Join(ownedfile.ErrRetirement, err)
	}
	r.bank = bank
	return nil
}

// EnablePhysicalTerminalQualified consumes an independent common-selection
// fingerprint and authenticated native recovery result. Existing candidates
// grant no permission; only a valid receipt paired with actual birth identities
// can reclaim their aliases. Its returned upper bound remains charged by the
// actual native growth owner until joined target-tail retirement.
func (s *Store) EnablePhysicalTerminalQualified(ctx context.Context, selection [32]byte, native PhysicalNativeReference, reserveNative func(PhysicalNativeReference, int64) error) (int64, error) {
	if selection == ([32]byte{}) || native.File.Inode == 0 || native.Parent.Inode == 0 {
		return 0, ErrSealIneligible
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enablePhysicalTerminalQualifiedLocked(ctx, selection, native, reserveNative)
}

func (s *Store) enablePhysicalTerminalQualifiedLocked(ctx context.Context, selection [32]byte, native PhysicalNativeReference, reserveNative func(PhysicalNativeReference, int64) error) (int64, error) {
	if s.closed || s.closing || s.readOnly || s.physical != nil {
		return 0, ErrRecovery
	}
	if err := errors.Join(ctx.Err(), s.checkPack(), s.checkRoot(), s.fault()); err != nil {
		return 0, err
	}
	if err := s.checkUnqualifiedPhysicalSlotsLocked(); err != nil {
		return 0, err
	}
	// Include all four bounded opaque preservation slots before any mutation.
	// Persistent receipt ownership cannot borrow a callback-scoped RAM slot.
	fee, err := s.budget.TryAcquireContext(resourcebudget.WithoutRAMWorkspace(ctx), resourcebudget.Background, resourcebudget.Cost{RAM: 32 << 10, Disk: (5*(maxPhysicalTerminalSlots+2) + 5) * 4096})
	if err != nil {
		return 0, err
	}
	s.physical = &physicalStoreOwner{fee: fee, allocatedEnd: int64(s.physicalExtent)}
	end, err := s.openPhysicalReceiptLocked(selection, native, reserveNative)
	if err != nil {
		s.physical.failure = err
		s.poison(err)
		return 0, err
	}
	s.physicalRecoveryPending = false
	return end, nil
}

func (s *Store) openPhysicalReceiptLocked(selection [32]byte, native PhysicalNativeReference, reserveNative func(PhysicalNativeReference, int64) error) (int64, error) {
	pack, err := resourcebudget.CapturePhysicalIdentity(s.pack)
	if err != nil {
		return 0, err
	}
	parent, _, err := physicalPathIdentity(s.dir)
	if err != nil {
		return 0, err
	}
	r := &physicalReceipt{bank: 1}
	s.physical.receipt = r // every opened/created FD belongs to Close even on failure
	valid, present := false, false
	for i := 0; i < 2; i++ {
		raw, _, readErr := readFixed(s.receiptPath(i), physicalReceiptBytes)
		if !errors.Is(readErr, os.ErrNotExist) {
			present = true
		}
		if readErr != nil {
			continue
		}
		c, err := decodePhysicalReceipt(raw, s.key)
		if err == nil && (!valid || c.Serial > r.claims.Serial) {
			r.claims, r.bank, valid = c, i, true
		}
	}
	for i := 0; i < maxPhysicalTerminalSlots; i++ {
		if _, err := os.Lstat(s.slotPath(s.packSlot, fmt.Sprintf("terminal-root-%02d", i))); err == nil {
			present = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
	}
	if valid {
		c, err := s.qualifyPhysicalPublishedPendingLocked(r.claims)
		if err != nil {
			return 0, err
		}
		r.claims = c
		if err := s.checkPhysicalRecoveryPendingAliasLocked(c); err != nil {
			return 0, err
		}
		if c.Selection != selection || c.Scope != s.current.content.context || c.Pack != pack || c.Parent != parent || c.Native.File != native.File || c.Native.Parent != native.Parent || c.Native.Generation != native.Generation || c.Native.Sequence > native.Sequence || c.Native.Sequence == native.Sequence && c.Native.Hash != native.Hash || c.Native.EOF > native.EOF || c.COWEOF > int64(s.physicalExtent) {
			return 0, ErrSealIneligible
		}
		if reserveNative != nil {
			end := c.NativeEnd
			if end < native.EOF {
				end = native.EOF
			}
			if err := reserveNative(c.Native, end); err != nil {
				return 0, err
			}
		}
		packEnd := c.COWEnd
		if packEnd < int64(s.physicalExtent) {
			packEnd = int64(s.physicalExtent)
		}
		if err := resourcebudget.CheckPhysicalTail(s.pack, int64(s.physicalExtent), packEnd); err != nil {
			return 0, err
		}
		for i := 0; i < 2; i++ {
			id, info, err := physicalPathIdentity(s.receiptPath(i))
			if err != nil || id != c.Controls[i] || !info.Mode().IsRegular() || info.Size() != physicalReceiptBytes {
				return 0, errors.Join(ownedfile.ErrReplaced, err)
			}
			r.files[i], err = os.OpenFile(s.receiptPath(i), os.O_RDWR, 0)
			if err != nil {
				return 0, err
			}
			if err := resourcebudget.CheckPhysicalTail(r.files[i], physicalReceiptBytes, physicalReceiptBytes); err != nil {
				return 0, err
			}
			r.infos[i] = info
		}
		// All current business obligations were independently authenticated and
		// recovered before this caller. Retire only receipt-owned spare aliases;
		// a published active.root alias remains independently selected.
		var captured [maxPhysicalTerminalSlots]os.FileInfo
		var capturedPaths [maxPhysicalTerminalSlots]string
		unknown := false
		for i, expected := range c.Slots {
			path, err := s.physicalReceiptSlotPathLocked(c, i)
			if err != nil {
				return 0, err
			}
			if expected.Inode == 0 {
				if _, err := os.Lstat(path); err == nil {
					unknown = true
				} else if !errors.Is(err, os.ErrNotExist) {
					return 0, err
				}
				continue
			}
			actual, info, err := physicalPathIdentity(path)
			if err != nil || actual != expected || !info.Mode().IsRegular() || info.Size() > cowRootBytes {
				return 0, errors.Join(ownedfile.ErrReplaced, err)
			}
			f, err := os.Open(path)
			if err != nil {
				return 0, err
			}
			actualInfo, statErr := f.Stat()
			allocated, allocationErr := resourcebudget.PhysicalAllocatedBytes(f)
			closeErr := ownedfile.CloseFile(f)
			if statErr != nil || !os.SameFile(actualInfo, info) || allocated > 4096 || allocationErr != nil || closeErr != nil {
				return 0, errors.Join(ownedfile.ErrReplaced, statErr, allocationErr, closeErr)
			}
			captured[i], capturedPaths[i] = info, path
		}
		if unknown {
			// A crash after exclusive creation but before its receipt update
			// leaves an unowned candidate. Preserve the entire bounded control
			// group opaquely; the independently authenticated receipt still owns
			// only the native/COW upper ranges, never that unknown file.
			if err := s.closePhysicalReceiptFilesLocked(); err != nil {
				return 0, err
			}
			if err := s.preserveUnqualifiedPhysicalLocked(); err != nil {
				return 0, err
			}
			if c.COWEnd > int64(s.physicalExtent) {
				if err := resourcebudget.RetirePhysicalTail(s.pack, int64(s.physicalExtent), c.COWEnd); err != nil {
					return 0, err
				}
			}
			end := c.NativeEnd
			if end < native.EOF {
				end = native.EOF
			}
			r = &physicalReceipt{bank: 1}
			s.physical.receipt = r
			return s.initializePhysicalReceiptLocked(r, selection, native, pack, parent, end)
		}
		for i, info := range captured {
			if info == nil {
				continue
			}
			if err := retireCaptured(capturedPaths[i], &info); err != nil {
				return 0, err
			}
		}
		if err := syncDir(s.dir); err != nil {
			return 0, err
		}
		if c.COWEnd > int64(s.physicalExtent) {
			if err := resourcebudget.RetirePhysicalTail(s.pack, int64(s.physicalExtent), c.COWEnd); err != nil {
				return 0, err
			}
		}
		s.physical.receipt = r
		r.claims.Native = native
		if r.claims.NativeEnd < native.EOF {
			r.claims.NativeEnd = native.EOF
		}
		if err := s.savePhysicalReceiptLocked(); err != nil {
			return 0, err
		}
		return r.claims.NativeEnd, nil
	}
	if reserveNative != nil {
		if err := reserveNative(native, native.EOF); err != nil {
			return 0, err
		}
	}
	if err := resourcebudget.CheckPhysicalTail(s.pack, int64(s.physicalExtent), int64(s.physicalExtent)); err != nil {
		return 0, err
	}
	if present {
		if err := s.preserveUnqualifiedPhysicalLocked(); err != nil {
			return 0, err
		}
	}
	return s.initializePhysicalReceiptLocked(r, selection, native, pack, parent, native.EOF)
}

func (s *Store) initializePhysicalReceiptLocked(r *physicalReceipt, selection [32]byte, native PhysicalNativeReference, pack, parent resourcebudget.PhysicalIdentity, end int64) (int64, error) {
	r.claims = physicalReceiptClaims{Selection: selection, Scope: s.current.content.context, Native: native, NativeEnd: end, Pack: pack, Parent: parent}
	for i := 0; i < 2; i++ {
		f, owner, err := ownedfile.Create(s.receiptPath(i), os.O_RDWR, 0600)
		if err != nil {
			return 0, err
		}
		r.files[i], r.owners[i] = f, owner
		if err := s.phase(fmt.Sprintf("physical-control-created-%d", i)); err != nil {
			return 0, err
		}
		r.infos[i], err = f.Stat()
		if err != nil {
			return 0, err
		}
		r.claims.Controls[i], err = resourcebudget.CapturePhysicalIdentity(f)
		if err != nil {
			return 0, err
		}
		if err := resourcebudget.AllocatePhysicalRange(f, 0, physicalReceiptBytes); err != nil {
			return 0, err
		}
		if err := s.phase(fmt.Sprintf("physical-control-allocated-%d", i)); err != nil {
			return 0, err
		}
		if n, err := f.WriteAt(make([]byte, physicalReceiptBytes), 0); err != nil || n != physicalReceiptBytes {
			return 0, errors.Join(err, io.ErrShortWrite)
		}
		if err := f.Sync(); err != nil {
			return 0, err
		}
		if err := s.phase(fmt.Sprintf("physical-control-initialized-%d", i)); err != nil {
			return 0, err
		}
	}
	if err := syncDir(s.dir); err != nil {
		return 0, err
	}
	s.physical.receipt = r
	if err := s.savePhysicalReceiptLocked(); err != nil {
		return 0, err
	}
	if err := s.phase("physical-genesis-synced"); err != nil {
		return 0, err
	}
	return end, nil
}

// These opaque slots grant no ownership. Their actual inode charges must fit
// the finite envelope before another qualified preparation may be admitted.
func (s *Store) checkUnqualifiedPhysicalSlotsLocked() error {
	for slot := 0; slot < 4; slot++ {
		path := filepath.Join(s.dir, fmt.Sprintf("terminal-unqualified-%d", slot))
		captured, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !captured.IsDir() || captured.Mode()&os.ModeSymlink != 0 {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
		directory, err := os.Open(path)
		if err != nil {
			return err
		}
		actual, statErr := directory.Stat()
		entries, readErr := directory.Readdir(maxPhysicalTerminalSlots + 3)
		allocated, allocationErr := resourcebudget.PhysicalAllocatedBytes(directory)
		closeErr := ownedfile.CloseFile(directory)
		if statErr != nil || !os.SameFile(captured, actual) || readErr != nil && !errors.Is(readErr, io.EOF) || allocationErr != nil || closeErr != nil || allocated > 4096 || len(entries) > maxPhysicalTerminalSlots+2 {
			return errors.Join(ownedfile.ErrReplaced, statErr, readErr, allocationErr, closeErr)
		}
		for _, entry := range entries {
			limit := int64(-1)
			for bank := 0; bank < 2; bank++ {
				if entry.Name() == fmt.Sprintf("terminal.physical.%d", bank) {
					limit = physicalReceiptBytes
				}
			}
			for i := 0; i < maxPhysicalTerminalSlots; i++ {
				if entry.Name() == fmt.Sprintf("terminal-root-%02d", i) {
					limit = cowRootBytes
				}
			}
			if limit < 0 || !entry.Mode().IsRegular() || entry.Size() < 0 || entry.Size() > limit {
				return ownedfile.ErrReplaced
			}
			file, err := os.Open(filepath.Join(path, entry.Name()))
			if err != nil {
				return err
			}
			actual, statErr := file.Stat()
			allocated, allocationErr := resourcebudget.PhysicalAllocatedBytes(file)
			closeErr := ownedfile.CloseFile(file)
			if statErr != nil || !os.SameFile(entry, actual) || allocationErr != nil || closeErr != nil || allocated > 4096 {
				return errors.Join(ownedfile.ErrReplaced, statErr, allocationErr, closeErr)
			}
		}
		after, err := os.Lstat(path)
		if err != nil || !os.SameFile(after, captured) {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
	}
	return nil
}

func (s *Store) preserveUnqualifiedPhysicalLocked() error {
	var names [maxPhysicalTerminalSlots + 2]string
	var infos [maxPhysicalTerminalSlots + 2]os.FileInfo
	count := 0
	for i := 0; i < maxPhysicalTerminalSlots+2; i++ {
		path, limit := s.receiptPath(i-maxPhysicalTerminalSlots), int64(physicalReceiptBytes)
		if i < maxPhysicalTerminalSlots {
			path, limit = s.slotPath(s.packSlot, fmt.Sprintf("terminal-root-%02d", i)), cowRootBytes
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > limit {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		actual, statErr := f.Stat()
		allocated, allocationErr := resourcebudget.PhysicalAllocatedBytes(f)
		closeErr := ownedfile.CloseFile(f)
		if statErr != nil || allocationErr != nil || closeErr != nil || !os.SameFile(actual, info) || allocated > 4096 {
			return errors.Join(ownedfile.ErrReplaced, statErr, allocationErr, closeErr)
		}
		names[count], infos[count], count = path, info, count+1
	}
	destination := ""
	for i := 0; i < 4; i++ {
		path := filepath.Join(s.dir, fmt.Sprintf("terminal-unqualified-%d", i))
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			destination = path
			break
		} else if err != nil {
			return err
		}
	}
	if destination == "" {
		return ErrLimit
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return err
	}
	for i := 0; i < count; i++ {
		actual, err := os.Lstat(names[i])
		if err != nil || !os.SameFile(actual, infos[i]) || actual.Size() != infos[i].Size() {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
		if err := os.Rename(names[i], filepath.Join(destination, filepath.Base(names[i]))); err != nil {
			return err
		}
	}
	return errors.Join(syncDir(destination), syncDir(s.dir))
}

func (s *Store) RecordPhysicalNativeRange(native PhysicalNativeReference, end int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.physical == nil || s.physical.receipt == nil {
		return nil
	}
	r := s.physical.receipt
	if native.File != r.claims.Native.File || native.Parent != r.claims.Native.Parent || native.Generation != r.claims.Native.Generation || native.Sequence < r.claims.Native.Sequence || native.EOF < r.claims.Native.EOF || end < native.EOF || end > 1<<50 {
		return ErrRecovery
	}
	r.claims.Native, r.claims.NativeEnd = native, end
	err := s.savePhysicalReceiptLocked()
	if err != nil {
		s.physical.failure = err
		s.poison(err)
	}
	return err
}

// BindPhysicalSelectionDigest is called only after actual common publication.
// A previous receipt must already have joined retirement before changing it.
func (s *Store) BindPhysicalSelectionDigest(selection [32]byte, native PhysicalNativeReference) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.physical == nil {
		return nil
	}
	if selection == ([32]byte{}) || s.physical.debt != 0 || s.physical.windows != 0 {
		return ErrRecovery
	}
	var err error
	if s.physical.receipt != nil {
		err = s.bindRetainedPhysicalSelectionLocked(selection, native)
	} else {
		_, err = s.openPhysicalReceiptLocked(selection, native, nil)
	}
	if err != nil {
		s.physical.failure = err
		s.poison(err)
	}
	return err
}

// An actual owner calls this only after independent common publication and
// joined NoDebt retirement. It retains the same captured banks, fee and extents.
func (s *Store) bindRetainedPhysicalSelectionLocked(selection [32]byte, native PhysicalNativeReference) error {
	p := s.physical
	if p == nil || p.receipt == nil || p.failure != nil || p.debt != 0 || p.windows != 0 || selection == ([32]byte{}) {
		return ErrRecovery
	}
	for _, slot := range p.slots {
		if slot != nil {
			return ErrRecovery
		}
	}
	if err := errors.Join(s.checkPack(), s.checkRoot(), s.fault()); err != nil {
		return err
	}
	r := p.receipt
	root, err := s.current.encode(s.key)
	if err != nil || !bytes.Equal(root, r.claims.COWRoot[:]) || sha256.Sum256(root) != s.physicalRootDigest {
		return errors.Join(ErrRecovery, err)
	}
	if err := s.checkPhysicalReceiptControlsLocked(); err != nil {
		return err
	}
	r.claims.Selection, r.claims.Native = selection, native
	// Preserve native target obligations until its actual owner retires them.
	if r.claims.NativeEnd < native.EOF {
		r.claims.NativeEnd = native.EOF
	}
	return s.savePhysicalReceiptLocked()
}

func (s *Store) checkPhysicalReceiptControlsLocked() error {
	if s.physical == nil || s.physical.receipt == nil {
		return ErrRecovery
	}
	r := s.physical.receipt
	for i := 0; i < 2; i++ {
		id, info, err := physicalPathIdentity(s.receiptPath(i))
		if err != nil || id != r.claims.Controls[i] || !os.SameFile(info, r.infos[i]) {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
		actual, err := resourcebudget.CapturePhysicalIdentity(r.files[i])
		if err != nil || actual != id {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
		if err := errors.Join(resourcebudget.CheckPhysicalRange(r.files[i], 0, physicalReceiptBytes), resourcebudget.CheckPhysicalTail(r.files[i], physicalReceiptBytes, physicalReceiptBytes)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) EnsurePhysicalReceipt(selection [32]byte, native PhysicalNativeReference) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.physical == nil {
		return ErrRecovery
	}
	if s.physical.receipt != nil {
		return nil
	}
	if s.physical.debt != 0 || s.physical.windows != 0 {
		return ErrRecovery
	}
	_, err := s.openPhysicalReceiptLocked(selection, native, nil)
	if err != nil {
		s.physical.failure = err
		s.poison(err)
	}
	return err
}

func (s *Store) retirePhysicalReceiptLocked() error {
	if s.physical == nil || s.physical.receipt == nil {
		return nil
	}
	r := s.physical.receipt
	for i := 0; i < 2; i++ {
		if r.files[i] != nil {
			err := ownedfile.CloseFile(r.files[i])
			r.closeErr = errors.Join(r.closeErr, err)
			if err == nil {
				r.files[i] = nil
			}
		}
	}
	if r.closeErr != nil {
		return errors.Join(ownedfile.ErrRetirement, r.closeErr)
	}
	for i := 0; i < 2; i++ {
		id, info, err := physicalPathIdentity(s.receiptPath(i))
		if err != nil || id != r.claims.Controls[i] {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
		if err := retireCaptured(s.receiptPath(i), &info); err != nil {
			return err
		}
	}
	if err := syncDir(s.dir); err != nil {
		return err
	}
	s.physical.receipt = nil
	return nil
}

func (s *Store) closePhysicalReceiptFilesLocked() error {
	if s.physical == nil || s.physical.receipt == nil {
		return nil
	}
	r := s.physical.receipt
	for i := 0; i < 2; i++ {
		if r.files[i] != nil {
			err := ownedfile.CloseFile(r.files[i])
			r.closeErr = errors.Join(r.closeErr, err)
			if err == nil {
				r.files[i] = nil
			}
		}
	}
	return r.closeErr
}
