package authenticatedstate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
)

const repackOpaqueSlots = 4
const repackOpaqueDirectoryDisk int64 = 8 << 10
const repackOpaqueFileDisk int64 = 4 << 10

// RepackReservationBytes includes a second full pack, control files and four
// fixed opaque preservation slots. Only zero-byte pages and an intent bounded
// by repackIntentBytes qualify for preservation: bulk copying starts AFTER the
// complete authenticated intent is durable. Larger unknown packs stay intact
// and refuse automatic reclamation. This reservation does not allocate blocks.
func RepackReservationBytes(ceiling int64) (int64, error) {
	extra := int64(storeControlDisk+2*cowRootBytes+2*repackIntentBytes) + repackOpaqueSlots*(repackOpaqueDirectoryDisk+2*repackOpaqueFileDisk)
	if ceiling <= 0 || ceiling > math.MaxInt64-extra {
		return 0, ErrLimit
	}
	return ceiling + extra, nil
}

func repackOpaqueIdentity(info os.FileInfo, maxSize, maxBlocks int64, directory bool) error {
	if info == nil || info.Mode().Perm()&0077 != 0 || directory != info.IsDir() || !directory && !info.Mode().IsRegular() || info.Size() < 0 || !directory && info.Size() > maxSize {
		return ownedfile.ErrReplaced
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !directory && st.Nlink != 1 || st.Blocks < 0 || int64(st.Blocks) > maxBlocks/512 {
		return ErrLimit
	}
	return nil
}

func repackOpaqueNameMatches(path string, captured os.FileInfo) error {
	named, err := os.Lstat(path)
	if err != nil || !os.SameFile(captured, named) || captured.Size() != named.Size() || captured.Mode() != named.Mode() || !captured.ModTime().Equal(named.ModTime()) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	before, beforeOK := captured.Sys().(*syscall.Stat_t)
	after, afterOK := named.Sys().(*syscall.Stat_t)
	if !beforeOK || !afterOK || before.Nlink != after.Nlink || before.Blocks != after.Blocks {
		return ownedfile.ErrReplaced
	}
	return nil
}

// preserveUnqualifiedRepack operates under PrepareRepack's actual independent
// common selection and source checks. It never authenticates unknown bytes or
// grants deletion authority. Every occupied fixed directory consumes a slot;
// an interrupted move can consume another slot, but never reuses or overwrites
// an existing one. The reservation covers all four through process lifetime.
func (s *Store) preserveUnqualifiedRepack(ctx context.Context, slot uint8, pages, intent os.FileInfo) error {
	if slot == s.packSlot || pages == nil && intent == nil {
		return ErrRecovery
	}
	if pages != nil {
		if err := repackOpaqueIdentity(pages, 0, repackOpaqueFileDisk, false); err != nil {
			return err
		}
	}
	if intent != nil {
		if err := repackOpaqueIdentity(intent, repackIntentBytes, repackOpaqueFileDisk, false); err != nil {
			return err
		}
	}
	if err := errors.Join(ctx.Err(), s.checkPack(), s.checkRoot()); err != nil {
		return err
	}
	// Refuse any independently selectable/control-bearing inactive candidate.
	for _, name := range []string{"active.root", "origin.root", "root.pending"} {
		if _, err := os.Lstat(s.slotPath(slot, name)); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(ErrRecovery, err)
		}
	}
	var destination string
	for i := 0; i < repackOpaqueSlots; i++ {
		path := filepath.Join(s.dir, fmt.Sprintf("repack.unqualified-%d", i))
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			if destination == "" {
				destination = path
			}
			continue
		}
		if err != nil {
			return err
		}
		// Existing contents have no authority and are never parsed or reused.
		if err := repackOpaqueIdentity(info, 0, repackOpaqueDirectoryDisk, true); err != nil {
			return err
		}
	}
	if destination == "" {
		return fmt.Errorf("unqualified repack slots exhausted; explicit archival is required: %w", ErrLimit)
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return err
	}
	directory, err := os.Lstat(destination)
	if err != nil {
		return err
	}
	if err := repackOpaqueIdentity(directory, 0, repackOpaqueDirectoryDisk, true); err != nil {
		return err
	}
	if err := s.phase("repack-opaque-slot-created"); err != nil {
		return err
	}
	for _, item := range []struct {
		name string
		info os.FileInfo
	}{{"pages", pages}, {"repack.intent", intent}} {
		if item.info == nil {
			continue
		}
		if err := errors.Join(ctx.Err(), s.checkPack(), s.checkRoot()); err != nil {
			return err
		}
		namedDirectory, err := os.Lstat(destination)
		if err != nil || !os.SameFile(directory, namedDirectory) {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
		original := s.slotPath(slot, item.name)
		if err := repackOpaqueNameMatches(original, item.info); err != nil {
			return err
		}
		target := filepath.Join(destination, item.name)
		if err := renameRepackWithoutReplace(original, target); err != nil {
			return err
		}
		if err := repackOpaqueNameMatches(target, item.info); err != nil {
			return err
		}
		phase := "repack-opaque-pages-preserved"
		if item.name == "repack.intent" {
			phase = "repack-opaque-intent-preserved"
		}
		if err := s.phase(phase); err != nil {
			return err
		}
		if err := errors.Join(durable.SyncDirectory(destination), durable.SyncDirectory(s.dir)); err != nil {
			return err
		}
	}
	return errors.Join(s.checkPack(), s.checkRoot(), durable.SyncDirectory(destination), durable.SyncDirectory(s.dir))
}
