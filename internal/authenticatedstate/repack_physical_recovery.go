package authenticatedstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"syscall"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// The caller already authenticated an independently selected complete current
// root at the full native head and exhausted the original recovery streams.
// This receipt can qualify ownership of that exact copied root; it cannot
// promote a candidate root or forgive an unknown source replacement.
func (s *Store) qualifyPhysicalRepackNoDebtLocked(ctx context.Context, from physicalReceiptClaims, expectedCut [32]byte) (physicalReceiptClaims, error) {
	to := from
	if from.RepackSourceSlot > 1 || from.RepackSourceSlot == s.packSlot || from.RepackTargetPack.Inode == 0 || from.RepackTargetRootFile.Inode == 0 || from.RepackTargetEOF < headerBytes || from.RepackTargetEOF > s.ceiling || from.Debt != 0 {
		return to, ErrSealIneligible
	}
	for _, slot := range from.Slots {
		if slot.Inode != 0 {
			return to, ErrSealIneligible
		}
	}
	if err := ctx.Err(); err != nil {
		return to, err
	}
	pack, err := resourcebudget.CapturePhysicalIdentity(s.pack)
	if err != nil || pack != from.RepackTargetPack || int64(s.physicalExtent) != from.RepackTargetEOF || s.info.Size() != from.RepackTargetEOF {
		return to, errors.Join(ownedfile.ErrReplaced, err)
	}
	rootFile, _, err := physicalPathIdentity(s.slotPath(s.packSlot, "active.root"))
	raw, _, readErr := readFixed(s.slotPath(s.packSlot, "active.root"), cowRootBytes)
	if err != nil || readErr != nil || rootFile != from.RepackTargetRootFile || !bytes.Equal(raw, from.RepackTargetRoot[:]) || sha256.Sum256(raw) != s.rootDigest {
		return to, errors.Join(ErrSealIneligible, err, readErr)
	}
	target, err := decodeCOWRoot(raw, s.key, s.current.content.context, expectedCut)
	if err != nil || target != s.current {
		return to, errors.Join(ErrSealIneligible, err)
	}
	var originalCut [32]byte
	copy(originalCut[:], from.COWRoot[8:40])
	original, err := decodeCOWRoot(from.COWRoot[:], s.key, s.current.content.context, originalCut)
	if err != nil || originalCut != expectedCut || original.content.id != target.content.id || original.revision == ^uint64(0) || original.revision+1 != target.revision || from.COWEOF != int64(original.content.extent) {
		return to, errors.Join(ErrSealIneligible, err)
	}
	// The captured source remains named until Commit's actual transfer bank is
	// durable. A missing/replaced source is never accepted as successful exit.
	sourceRoot, _, err := physicalPathIdentity(s.slotPath(from.RepackSourceSlot, "active.root"))
	sourceRaw, _, readErr := readFixed(s.slotPath(from.RepackSourceSlot, "active.root"), cowRootBytes)
	if err != nil || readErr != nil || sourceRoot != from.COWRootFile || !bytes.Equal(sourceRaw, from.COWRoot[:]) {
		return to, errors.Join(ownedfile.ErrReplaced, err, readErr)
	}
	f, err := os.OpenFile(s.slotPath(from.RepackSourceSlot, "pages"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return to, err
	}
	actual, identityErr := resourcebudget.CapturePhysicalIdentity(f)
	info, statErr := f.Stat()
	rangeErr := resourcebudget.CheckPhysicalTail(f, from.COWEOF, from.COWEnd)
	closeErr := ownedfile.CloseFile(f)
	if identityErr != nil || statErr != nil || actual != from.Pack || info.Size() != from.COWEOF || rangeErr != nil || closeErr != nil {
		return to, errors.Join(ownedfile.ErrReplaced, identityErr, statErr, rangeErr, closeErr)
	}
	to.Pack, to.COWRootFile, to.COWRoot = from.RepackTargetPack, from.RepackTargetRootFile, from.RepackTargetRoot
	to.COWEOF, to.COWEnd = from.RepackTargetEOF, from.RepackTargetEOF
	clearPhysicalRepack(&to)
	return to, nil
}
