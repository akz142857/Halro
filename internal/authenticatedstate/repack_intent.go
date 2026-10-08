package authenticatedstate

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
)

const repackIntentBytes = 216

func repackFileIdentity(info os.FileInfo) ([16]byte, error) {
	var id [16]byte
	if info == nil {
		return id, ownedfile.ErrReplaced
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return id, ownedfile.ErrReplaced
	}
	binary.BigEndian.PutUint64(id[:8], uint64(st.Dev))
	binary.BigEndian.PutUint64(id[8:], uint64(st.Ino))
	return id, nil
}

// The receipt records an exclusively created descriptor before bulk copying.
// It grants deletion of that exact inode only, never selection of its contents.
// The complete independently selected source and original directory guard are
// checked by PrepareRepack before this receipt can be created.
func (s *Store) createRepackIntent(slot uint8, pack os.FileInfo) (*ownedfile.Owner, error) {
	parent, err := repackFileIdentity(s.dirInfo)
	if err != nil {
		return nil, err
	}
	source, err := repackFileIdentity(s.info)
	if err != nil {
		return nil, err
	}
	target, err := repackFileIdentity(pack)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, repackIntentBytes)
	copy(raw[:8], "HLRPINT1")
	copy(raw[8:40], s.current.content.context[:])
	copy(raw[40:56], s.current.content.id[:])
	binary.BigEndian.PutUint64(raw[56:64], s.physicalRevision)
	raw[64], raw[65] = s.packSlot, slot
	copy(raw[72:88], parent[:])
	copy(raw[88:104], source[:])
	copy(raw[104:120], target[:])
	copy(raw[120:152], s.current.cut[:])
	copy(raw[152:184], s.rootDigest[:])
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("halro:authenticated-state:repack-intent:v1\x00"))
	mac.Write(raw[:184])
	copy(raw[184:], mac.Sum(nil))
	return s.writePreparationIntent(s.slotPath(slot, "repack.intent"), raw)
}

func (s *Store) readRepackIntent(slot uint8, pack os.FileInfo) ([]byte, os.FileInfo, error) {
	raw, info, err := readFixed(s.slotPath(slot, "repack.intent"), repackIntentBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, info, err
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("halro:authenticated-state:repack-intent:v1\x00"))
	mac.Write(raw[:184])
	parent, identityErr := repackFileIdentity(s.dirInfo)
	if identityErr != nil || !bytes.Equal(raw[:8], []byte("HLRPINT1")) || !hmac.Equal(raw[184:], mac.Sum(nil)) ||
		!bytes.Equal(raw[8:40], s.current.content.context[:]) || !bytes.Equal(raw[40:56], s.current.content.id[:]) ||
		!bytes.Equal(raw[72:88], parent[:]) || raw[64] > 1 || raw[65] != slot || raw[64] == slot ||
		!bytes.Equal(raw[66:72], make([]byte, 6)) || binary.BigEndian.Uint64(raw[56:64]) == 0 {
		return nil, info, errors.Join(ErrRecovery, identityErr)
	}
	if pack != nil {
		target, err := repackFileIdentity(pack)
		if err != nil || !bytes.Equal(raw[104:120], target[:]) {
			return nil, info, errors.Join(ownedfile.ErrReplaced, err)
		}
	}
	return raw, info, nil
}

// Try retiring a partial copy only while its original source inode is still
// the independently selected pack. A replacement, incomplete receipt/control,
// or later source generation refuses cleanup and preserves every unknown file.
func (s *Store) retireInterruptedRepack(ctx context.Context, slot uint8, raw []byte, info os.FileInfo, captured [4]os.FileInfo) (bool, error) {
	if raw == nil || raw[64] != s.packSlot {
		return false, nil
	}
	source, err := repackFileIdentity(s.info)
	if err != nil || !bytes.Equal(raw[88:104], source[:]) {
		return false, err
	}
	revision := binary.BigEndian.Uint64(raw[56:64])
	if revision > s.current.revision || revision == ^uint64(0) {
		return false, ErrRecovery
	}
	var cut [32]byte
	copy(cut[:], raw[120:152])
	for i, name := range [4]string{"pages", "active.root", "origin.root", "root.pending"} {
		if i == 0 || captured[i] == nil {
			continue
		}
		control, actual, err := readFixed(s.slotPath(slot, name), cowRootBytes)
		if err != nil || !os.SameFile(actual, captured[i]) {
			return false, errors.Join(ErrRecovery, err)
		}
		root, err := decodeCOWRoot(control, s.key, s.current.content.context, cut)
		if err != nil || root.content.id != s.current.content.id || root.revision != revision+1 {
			return false, errors.Join(ErrRecovery, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	for _, i := range [4]int{0, 2, 3, 1} {
		name := [4]string{"pages", "active.root", "origin.root", "root.pending"}[i]
		if err := removeCaptured(s.slotPath(slot, name), captured[i]); err != nil {
			return false, err
		}
	}
	// Keep the authenticated receipt until every original named inode is gone.
	if err := durable.SyncDirectory(s.dir); err != nil {
		return false, err
	}
	if err := removeCaptured(s.slotPath(slot, "repack.intent"), info); err != nil {
		return false, err
	}
	return true, durable.SyncDirectory(s.dir)
}

// The partial-write checkpoints cover real first-run file creation, before
// the receipt is eligible to authorize deletion of a bulk-copy candidate.
func (s *Store) writePreparationIntent(path string, raw []byte) (owner *ownedfile.Owner, resultErr error) {
	f, owner, err := ownedfile.Create(path, os.O_RDWR, 0600)
	if err != nil {
		return owner, err
	}
	defer func() { resultErr = errors.Join(resultErr, ownedfile.CloseFile(f)) }()
	if err := s.phase("repack-intent-created"); err != nil {
		return owner, err
	}
	for i, part := range [][]byte{raw[:len(raw)/2], raw[len(raw)/2:]} {
		if n, err := f.Write(part); err != nil || n != len(part) {
			return owner, errors.Join(err, io.ErrShortWrite)
		}
		phase := "repack-intent-partially-written"
		if i == 1 {
			phase = "repack-intent-written"
		}
		if err := s.phase(phase); err != nil {
			return owner, err
		}
	}
	return owner, errors.Join(f.Sync(), owner.VerifyFile(f), durable.SyncDirectory(s.dir))
}
