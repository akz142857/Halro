package ledger

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
)

const segmentDirectoryIndexName = "segments.idx"

var segmentDirectoryIndexMagic = [8]byte{'H', 'L', 'R', 'O', 'S', 'D', 'I', '1'}

// buildRuntimeSegmentDirectoryIndex reuses an authenticated persistent ordinal
// locator when it matches the exact pinned manifest bytes. The manifest stays
// authoritative; missing, stale, or damaged locator files are rebuilt by
// streaming the manifest and are never interpreted as a shorter history.
func buildRuntimeSegmentDirectoryIndex(directory string, maxBytes int64, key []byte) (*segmentDirectoryIndex, error) {
	return buildRuntimeSegmentDirectoryIndexWithDisk(directory, maxBytes, key, nil)
}

func buildRuntimeSegmentDirectoryIndexWithDisk(directory string, maxBytes int64, key []byte, reserve reserveNativeFileDisk) (*segmentDirectoryIndex, error) {
	if index, err := openPersistentSegmentDirectoryIndex(directory, maxBytes, key); err != nil || index != nil {
		return index, err
	}
	if len(key) != 32 {
		return buildSegmentDirectoryIndexInternalWithDisk(directory, os.TempDir(), maxBytes, nil, false, reserve)
	}
	return buildSegmentDirectoryIndexInternalWithDisk(directory, os.TempDir(), maxBytes, key, true, reserve)
}

// Reader locators share a valid persistent sidecar but never publish one. A
// reader seeing a stale manifest gets its own bounded scratch cursor; the Log
// owner alone replaces the sidecar at its manifest commit boundary.
func buildReaderSegmentDirectoryIndex(directory string, maxBytes int64, key []byte) (*segmentDirectoryIndex, error) {
	if len(key) == 32 {
		if index, err := openPersistentSegmentDirectoryIndex(directory, maxBytes, key); err != nil || index != nil {
			return index, err
		}
	}
	return buildSegmentDirectoryIndexWithLimit(directory, os.TempDir(), maxBytes)
}

func openPersistentSegmentDirectoryIndex(directory string, maxBytes int64, key []byte) (*segmentDirectoryIndex, error) {
	if maxBytes < 0 {
		return nil, errors.New("Ledger segment directory index disk limit cannot be negative")
	}
	manifest, err := openInspectionManifest(directory)
	if err != nil {
		return nil, err
	}
	if manifest.file == nil {
		if _, statErr := os.Lstat(filepath.Join(directory, segmentDirectoryIndexName)); statErr == nil {
			return nil, errors.Join(fmt.Errorf("%w: Ledger manifest is missing while its authenticated directory locator remains", ErrCorrupt), manifest.close())
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, errors.Join(statErr, manifest.close())
		}
		return nil, manifest.close()
	}
	if len(key) != 32 {
		return nil, manifest.close()
	}
	path := filepath.Join(directory, segmentDirectoryIndexName)
	named, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, manifest.close()
	}
	if err != nil || !named.Mode().IsRegular() {
		return nil, errors.Join(ErrCorrupt, err, manifest.close())
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, manifest.close()
	}
	if err != nil {
		return nil, errors.Join(err, manifest.close())
	}
	retire := func(cause error) error {
		closeErr := errors.Join(ownedfile.CloseFile(file), manifest.close())
		if closeErr != nil {
			closeErr = errors.Join(ErrSegmentRetirement, closeErr)
		}
		return errors.Join(cause, closeErr)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, retire(err)
	}
	if !info.Mode().IsRegular() || !os.SameFile(info, named) {
		return nil, retire(fmt.Errorf("%w: Ledger directory locator sidecar is not a regular file", ErrCorrupt))
	}
	fail := func() (*segmentDirectoryIndex, error) {
		// Borrowed cache bytes never grant unlink authority. A rebuilt locator
		// replaces the exact captured inode only at its publish boundary.
		closeErr := errors.Join(ownedfile.CloseFile(file), manifest.close())
		if closeErr != nil {
			return nil, errors.Join(ErrSegmentRetirement, closeErr)
		}
		return nil, nil
	}

	if info.Size() < segmentDirectoryHeaderBytes {
		return fail()
	}
	var header [segmentDirectoryHeaderBytes]byte
	if _, err := file.ReadAt(header[:], 0); err != nil || string(header[:8]) != string(segmentDirectoryIndexMagic[:]) {
		return fail()
	}
	derivedKey := segmentDirectoryIndexKey(key)
	mac := hmac.New(sha256.New, derivedKey[:])
	_, _ = mac.Write(header[:80])
	if !hmac.Equal(mac.Sum(nil)[:16], header[80:96]) {
		clear(derivedKey[:])
		return fail()
	}
	count := binary.BigEndian.Uint64(header[8:16])
	manifestSize := binary.BigEndian.Uint64(header[16:24])
	version := binary.BigEndian.Uint64(header[72:80])
	if version != segmentManifestVersion || count > uint64(math.MaxInt64-segmentDirectoryHeaderBytes)/segmentDirectorySlotBytes || manifestSize != uint64(manifest.info.Size()) {
		clear(derivedKey[:])
		return fail()
	}
	needed := uint64(segmentDirectoryHeaderBytes) + count*segmentDirectorySlotBytes
	if uint64(info.Size()) != needed || (maxBytes > 0 && needed > uint64(maxBytes)) {
		clear(derivedKey[:])
		return fail()
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, io.NewSectionReader(manifest.file, 0, manifest.info.Size())); err != nil {
		clear(derivedKey[:])
		return nil, retire(err)
	}
	if !hmac.Equal(digest.Sum(nil), header[24:56]) {
		clear(derivedKey[:])
		return fail()
	}
	plain, stored := binary.BigEndian.Uint64(header[56:64]), binary.BigEndian.Uint64(header[64:72])
	if plain > math.MaxInt64 || stored > math.MaxInt64 {
		clear(derivedKey[:])
		return fail()
	}
	slotReader := bufio.NewReaderSize(io.NewSectionReader(file, segmentDirectoryHeaderBytes, int64(count)*segmentDirectorySlotBytes), 64*1024)
	var slot [segmentDirectorySlotBytes]byte
	for generation := uint64(1); generation <= count; generation++ {
		if _, err := io.ReadFull(slotReader, slot[:]); err != nil {
			clear(derivedKey[:])
			return fail()
		}
		mac := segmentDirectorySlotMAC(derivedKey, generation, slot[:segmentDirectorySlotMACOffset])
		if !hmac.Equal(mac[:], slot[segmentDirectorySlotMACOffset:]) {
			clear(derivedKey[:])
			return fail()
		}
	}
	parentPath, err := filepath.Abs(directory)
	if err != nil {
		clear(derivedKey[:])
		return nil, retire(err)
	}
	index := &segmentDirectoryIndex{
		manifest: manifest, index: file, indexInfo: info, key: derivedKey,
		parsed:   parsedSegmentManifest{version: int(version), count: count, array: true},
		summary:  SegmentSummary{Generations: count, PlainBytes: int64(plain), StoredBytes: int64(stored)},
		maxBytes: maxBytes, slotBase: segmentDirectoryHeaderBytes, persistent: true,
		readPermit: make(chan struct{}, 1), parent: parentPath, parentInfo: manifest.parentInfo,
	}
	if count > 0 {
		segment, found, getErr := index.Get(count)
		if getErr != nil || !found {
			return nil, index.Close()
		}
		index.parsed.last = segment
	}
	if err := manifest.check(); err != nil {
		closeErr := index.Close()
		if errors.Is(err, errInspectionManifestStale) {
			return nil, closeErr
		}
		return nil, errors.Join(err, closeErr)
	}
	if err := errors.Join(index.checkParent(), unchangedSegmentDescriptor(file, info), unchangedSegment(path, info)); err != nil {
		return nil, errors.Join(err, index.Close())
	}
	return index, nil
}

func publishPersistentSegmentDirectoryIndex(directory string, index *segmentDirectoryIndex) error {
	return publishPersistentSegmentDirectoryIndexWithSync(directory, index, durable.SyncDirectory)
}

func publishPersistentSegmentDirectoryIndexWithSync(directory string, index *segmentDirectoryIndex, syncDirectory func(string) error) (err error) {
	if index == nil || index.manifest == nil || index.manifest.file == nil || index.parsed.pending != nil {
		return errors.New("Ledger segment directory index requires a committed manifest")
	}
	if err := index.manifest.check(); err != nil {
		return err
	}
	needed := uint64(segmentDirectoryHeaderBytes) + index.parsed.count*segmentDirectorySlotBytes
	if index.maxBytes > 0 && needed > uint64(index.maxBytes) {
		return fmt.Errorf("Ledger segment directory index needs %d bytes, exceeding configured %d-byte ceiling", needed, index.maxBytes)
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, io.NewSectionReader(index.manifest.file, 0, index.manifest.info.Size())); err != nil {
		return err
	}
	temporary := index.index
	var header [segmentDirectoryHeaderBytes]byte
	copy(header[:8], segmentDirectoryIndexMagic[:])
	binary.BigEndian.PutUint64(header[8:16], index.parsed.count)
	binary.BigEndian.PutUint64(header[16:24], uint64(index.manifest.info.Size()))
	copy(header[24:56], digest.Sum(nil))
	binary.BigEndian.PutUint64(header[56:64], uint64(index.summary.PlainBytes))
	binary.BigEndian.PutUint64(header[64:72], uint64(index.summary.StoredBytes))
	binary.BigEndian.PutUint64(header[72:80], uint64(index.parsed.version))
	mac := hmac.New(sha256.New, index.key[:])
	_, _ = mac.Write(header[:80])
	copy(header[80:], mac.Sum(nil)[:16])
	if _, err := temporary.WriteAt(header[:], 0); err != nil {
		return err
	}
	if index.diskToken != nil {
		if err := verifyNativeFileEnvelope(temporary, index.diskEnvelope); err != nil {
			return err
		}
	}
	if err := index.manifest.check(); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := errors.Join(index.checkParent(), index.owner.VerifyFile(temporary)); err != nil {
		return err
	}
	destination := filepath.Join(index.parent, segmentDirectoryIndexName)
	if index.targetInfo != nil {
		if err := index.owner.ReplaceCaptured(destination, index.targetInfo); err != nil {
			return err
		}
		index.persistent = true // Rename happened; no subsequent error erases it.
	} else {
		alias, err := index.owner.LinkCaptured(destination)
		if alias != nil {
			index.temporaryAlias, index.owner = index.owner, alias
			index.persistent = true
		}
		if err != nil {
			return err
		}
	}
	if err := errors.Join(syncDirectory(directory), index.checkParent(), index.owner.VerifyFile(temporary)); err != nil {
		return err
	}
	// Reopen only the published inode, retaining the original live FD until
	// the replacement has been verified. On failure Close still joins it and
	// preserves the canonical published cache.
	file, err := os.OpenFile(destination, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	if err := index.owner.VerifyFile(file); err != nil {
		closeErr := ownedfile.CloseFile(file)
		if closeErr != nil {
			index.closeErr = errors.Join(index.closeErr, ErrSegmentRetirement, closeErr)
		}
		return errors.Join(err, closeErr)
	}
	info, statErr := file.Stat()
	if statErr != nil {
		closeErr := ownedfile.CloseFile(file)
		if closeErr != nil {
			index.closeErr = errors.Join(index.closeErr, ErrSegmentRetirement, closeErr)
		}
		return errors.Join(statErr, closeErr)
	}
	closeErr := ownedfile.CloseFile(temporary)
	index.index, index.indexInfo, index.slotBase = file, info, segmentDirectoryHeaderBytes
	if closeErr != nil {
		index.closeErr = errors.Join(index.closeErr, ErrSegmentRetirement, closeErr)
		return index.closeErr
	}
	if index.temporaryAlias != nil {
		if err := index.temporaryAlias.Retire(); err != nil {
			return errors.Join(ErrSegmentRetirement, err)
		}
		index.temporaryAlias = nil
		if err := durable.SyncDirectory(index.parent); err != nil {
			return err
		}
	}
	return nil
}

func segmentDirectoryIndexKey(key []byte) (result [32]byte) {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("halro:persistent-segment-directory-index:v1"))
	copy(result[:], mac.Sum(nil))
	return result
}
