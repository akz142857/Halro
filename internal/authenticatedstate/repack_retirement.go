package authenticatedstate

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"syscall"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
)

// Capture only this job's original source and fixed controls, after its full
// reachable-tree authentication/copy. Startup without this receipt still uses
// retireInactivePack's independent authenticated audit.
func (s *Store) captureRepackRetirement(ctx context.Context) ([5]os.FileInfo, error) {
	var captured [5]os.FileInfo
	if err := errors.Join(ctx.Err(), s.checkPack(), s.checkRoot()); err != nil {
		return captured, err
	}
	actual, err := s.pack.Stat()
	if err != nil || !os.SameFile(actual, s.info) {
		return captured, errors.Join(ownedfile.ErrReplaced, err)
	}
	captured[0] = actual
	for i, name := range []string{"active.root", "origin.root", "root.pending", "repack.intent"} {
		info, err := os.Lstat(s.slotPath(s.packSlot, name))
		if errors.Is(err, os.ErrNotExist) && i != 0 {
			continue
		}
		if err != nil {
			return captured, err
		}
		captured[i+1] = info
	}
	if err := s.validateRepackRetirement(s.packSlot, captured, s.physicalRootDigest, s.packDigest, s.physicalExtent, s.physicalRevision); err != nil {
		return captured, err
	}
	return captured, nil
}

// A constant-size preflight checks the original captured inode set, physical
// root commitment and pack header. It cannot adopt a newly discovered inode or
// turn an unexplained missing name into a successful unlink receipt.
func (s *Store) validateRepackRetirement(slot uint8, captured [5]os.FileInfo, rootDigest, packDigest [32]byte, extent, revision uint64) error {
	names := [5]string{"pages", "active.root", "origin.root", "root.pending", "repack.intent"}
	for i, name := range names {
		path := s.slotPath(slot, name)
		if captured[i] == nil {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				return errors.Join(ownedfile.ErrReplaced, err)
			}
			continue
		}
		if err := repackOpaqueNameMatches(path, captured[i]); err != nil {
			return err
		}
		limit := int64(cowRootBytes)
		if i == 0 {
			limit = s.ceiling
		}
		if i == 4 {
			limit = repackIntentBytes
		}
		// Private controls and pack must remain exclusively named. Size/block
		// fees of the full known source remain under its original reservation.
		if err := repackOpaqueIdentity(captured[i], limit, math.MaxInt64, false); err != nil {
			return err
		}
	}
	if captured[0] == nil || captured[1] == nil || captured[3] != nil {
		return ErrRecovery
	}
	if captured[0].Size() < int64(extent) {
		return ErrRecovery
	}
	header, err := readPackHeader(s.slotPath(slot, "pages"), captured[0])
	if err != nil || sha256.Sum256(header) != packDigest {
		return errors.Join(ErrRecovery, err)
	}
	initial, err := decodeCommitment(header, s.key, s.current.content.context)
	if err != nil || initial.id != s.current.content.id {
		return errors.Join(ErrRecovery, err)
	}
	raw, info, err := readFixed(s.slotPath(slot, "active.root"), cowRootBytes)
	if err != nil || !os.SameFile(info, captured[1]) || sha256.Sum256(raw) != rootDigest {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	var cut [32]byte
	copy(cut[:], raw[8:40])
	physical, err := decodeCOWRoot(raw, s.key, s.current.content.context, cut)
	if err != nil || physical.content.id != s.current.content.id || physical.revision != revision {
		return errors.Join(ErrRecovery, err)
	}
	var originRevision uint64
	if captured[2] != nil {
		origin, info, err := readFixed(s.slotPath(slot, "origin.root"), cowRootBytes)
		if err != nil || !os.SameFile(info, captured[2]) {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
		copy(cut[:], origin[8:40])
		first, err := decodeCOWRoot(origin, s.key, s.current.content.context, cut)
		if err != nil || first.content.id != physical.content.id || first.revision > physical.revision {
			return errors.Join(ErrRecovery, err)
		}
		originRevision = first.revision
	} else if slot != 0 {
		return ErrRecovery
	}
	intent, info, err := s.readRepackIntent(slot, captured[0])
	if err != nil {
		return err
	}
	if captured[4] == nil {
		if intent != nil || captured[2] != nil {
			return ErrRecovery
		}
	} else if intent == nil || !os.SameFile(info, captured[4]) || originRevision == 0 || binary.BigEndian.Uint64(intent[56:64])+1 != originRevision {
		return ErrRecovery
	}
	return nil
}

// New common selection and the copied reachable tree authorize this exact old
// job source. Close and inode/control preflight precede every unlink; an error
// retains both persistent fees and the generation's recovery requirement.
func (c *StoreRepack) retireCapturedSource() error {
	s := c.store
	if err := errors.Join(s.checkPack(), s.checkRoot(), c.publisher.ValidateOwner(c.guard, s.budget)); err != nil {
		return err
	}
	if err := s.validateRepackRetirement(c.old.slot, c.old.controls, c.old.physicalDigest, c.old.packDigest, c.old.extent, c.old.revision); err != nil {
		return err
	}
	for _, i := range [5]int{0, 2, 3, 1, 4} {
		name := [5]string{"pages", "active.root", "origin.root", "root.pending", "repack.intent"}[i]
		if err := errors.Join(s.checkPack(), s.checkRoot()); err != nil {
			return err
		}
		if c.old.controls[i] != nil {
			if err := repackOpaqueNameMatches(s.slotPath(c.old.slot, name), c.old.controls[i]); err != nil {
				return err
			}
		}
		if err := retireCaptured(s.slotPath(c.old.slot, name), &c.old.controls[i]); err != nil {
			return err
		}
	}
	return durable.SyncDirectory(s.dir)
}

func readPackHeader(path string, expected os.FileInfo) (raw []byte, resultErr error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, ownedfile.CloseFile(f)) }()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(expected, actual) {
		return nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	raw = make([]byte, headerBytes)
	if _, err := io.ReadFull(f, raw); err != nil {
		return nil, err
	}
	if err := repackOpaqueNameMatches(path, expected); err != nil {
		return nil, err
	}
	return raw, nil
}
