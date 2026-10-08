package ledger

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/durable"
)

// A pending manifest is not authority. Before changing any native name, prove
// the whole actual chain and its independently selected saved anchor. Recovery
// is exceptional O(history) IO, with one frame/descriptor/reader at a time.
func (s *selectedNativeStartup) recoverInterruptedRoll(d *segmentDirectoryIndex, directory string, maxIndex int64, key []byte, reserve reserveNativeFileDisk) (resultErr error) {
	pending := *d.parsed.pending
	if err := s.validateDirectoryEntries(d, pending.Generation); err != nil {
		return err
	}
	if pending.File != fmt.Sprintf("ledger-%d.wal", pending.Generation) || pending.Compressed || pending.Length <= 0 || pending.StoredLength != pending.Length || pending.PlainChecksum != pending.StoredChecksum {
		return ErrProjectionSource
	}
	parent, err := os.Lstat(directory)
	if err != nil || !parent.IsDir() {
		return errors.Join(ErrProjectionSource, err)
	}
	activePath, rolledPath := s.nativePath, filepath.Join(directory, pending.File)
	active, activeErr := os.Lstat(activePath)
	rolled, rolledErr := os.Lstat(rolledPath)
	if activeErr != nil && !errors.Is(activeErr, os.ErrNotExist) || rolledErr != nil && !errors.Is(rolledErr, os.ErrNotExist) {
		return errors.Join(ErrProjectionSource, activeErr, rolledErr)
	}
	renamed := rolledErr == nil
	if renamed {
		if !rolled.Mode().IsRegular() || activeErr == nil && (!active.Mode().IsRegular() || active.Size() != 0) {
			return ErrProjectionSource
		}
	} else if activeErr != nil || !active.Mode().IsRegular() {
		return errors.Join(ErrProjectionSource, activeErr)
	}
	check := func() error {
		currentParent, err := os.Lstat(directory)
		if err != nil || !currentParent.IsDir() || !os.SameFile(parent, currentParent) {
			return errors.Join(ErrProjectionSource, err)
		}
		for _, captured := range []struct {
			path string
			info os.FileInfo
		}{{activePath, active}, {rolledPath, rolled}} {
			named, err := os.Lstat(captured.path)
			if captured.info == nil {
				if !errors.Is(err, os.ErrNotExist) {
					return errors.Join(ErrProjectionSource, err)
				}
			} else if err != nil || !named.Mode().IsRegular() || !os.SameFile(captured.info, named) || captured.info.Size() != named.Size() || !captured.info.ModTime().Equal(named.ModTime()) {
				return errors.Join(ErrProjectionSource, err)
			}
		}
		return errors.Join(d.manifest.check(), s.checkSelection(), s.ctx.Err())
	}
	if err := check(); err != nil {
		return err
	}
	v := &chainVerifier{key: key}
	savedSeen := false
	for generation := uint64(1); generation <= pending.Generation; generation++ {
		segment := pending
		if generation < pending.Generation {
			var found bool
			segment, found, err = d.Get(generation)
			if err != nil || !found {
				return errors.Join(ErrSegmentMissing, err)
			}
		} else if !renamed {
			segment.File = filepath.Base(s.nativePath)
		}
		start, err := decodeChainHash(segment.StartHash)
		if err != nil || start != v.hash || segment.FirstSequence != v.sequence+1 || segment.FirstSequence == 0 {
			return errors.Join(ErrTampered, err)
		}
		if generation == s.anchor.Head.Generation && s.anchor.Head.Offset == 0 {
			savedSeen = s.anchor.Head.Sequence == v.sequence && s.anchor.Hash == v.hash && s.anchor.Epoch == v.epoch
			if !savedSeen {
				return ErrProjectionSource
			}
		}
		if err := s.verifyRolledDescriptor(directory, segment); err != nil {
			return err
		}
		if err := s.verifyPendingGeneration(directory, segment, v, func(r Record) error {
			if r.Generation == s.anchor.Head.Generation && r.Offset == s.anchor.Head.Offset {
				if r.Sequence != s.anchor.Head.Sequence || r.Hash != s.anchor.Hash || r.Epoch != s.anchor.Epoch {
					return ErrProjectionSource
				}
				savedSeen = true
			}
			return s.ctx.Err()
		}); err != nil {
			return err
		}
	}
	if !savedSeen {
		return ErrProjectionSource
	}
	if err := s.pendingPhase("selected-roll-qualified"); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	var appendEntry *Segment
	if renamed {
		if err := checkSegmentDirectoryIndexLimit(d.parsed.count+1, maxIndex); err != nil {
			return err
		}
		// Only an independently qualified renamed generation licenses an empty
		// successor. An existing successor is checked above, never truncated.
		flags := os.O_RDWR | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
		if active == nil {
			flags |= os.O_CREATE | os.O_EXCL
		}
		file, err := openNativeActiveWithDisk(activePath, flags, reserve)
		if err != nil {
			return err
		}
		info, statErr := file.Stat()
		named, nameErr := os.Lstat(activePath)
		if statErr != nil || nameErr != nil || !info.Mode().IsRegular() || info.Size() != 0 || !named.Mode().IsRegular() || !os.SameFile(info, named) || active != nil && !os.SameFile(active, info) {
			retireSelectedStartupFile(file, s)
			return errors.Join(ErrProjectionSource, statErr, nameErr)
		}
		active = info
		if err := s.pendingPhase("selected-roll-successor-created"); err != nil {
			retireSelectedStartupFile(file, s)
			return err
		}
		syncErr := errors.Join(file.Sync(), durable.SyncDirectory(directory))
		retireSelectedStartupFile(file, s)
		if err := errors.Join(syncErr, s.retirementErr, s.pendingPhase("selected-roll-successor-synced")); err != nil {
			return err
		}
		appendEntry = &pending
	}
	if err := check(); err != nil {
		return err
	}
	if _, err := saveSegmentManifestSourceWithDisk(directory, d.source(appendEntry, nil, nil), durable.SyncDirectory, reserve); err != nil {
		return err
	}
	return s.pendingPhase("selected-roll-manifest-published")
}

func (s *selectedNativeStartup) pendingPhase(stage string) error {
	if s.recoveryPhase != nil {
		return s.recoveryPhase(stage)
	}
	return s.ctx.Err()
}

func (s *selectedNativeStartup) verifyPendingGeneration(directory string, segment Segment, v *chainVerifier, visit func(Record) error) (resultErr error) {
	path := filepath.Join(directory, segment.File)
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != segment.StoredLength {
		retireSelectedStartupFile(file, s)
		return errors.Join(ErrProjectionSource, err)
	}
	var input io.ReadCloser = file
	if segment.Compressed {
		gz, err := gzip.NewReader(file)
		if err != nil {
			retireSelectedStartupFile(file, s)
			return errors.Join(ErrTampered, err)
		}
		input = gzipReadCloser{Reader: gz, under: file}
	}
	reader := &segmentReader{open: func() (io.ReadCloser, error) { return input, nil }}
	defer func() {
		if err := reader.Close(); err != nil {
			s.retainedSegment = reader
			s.retirementErr = errors.Join(s.retirementErr, ErrSegmentRetirement, err)
			resultErr = errors.Join(resultErr, err)
		}
	}()
	v.offset = 0
	head, partial, scanErr := scan(reader, segment.Generation, 0, v.sequence, visit, v)
	actual, statErr := file.Stat()
	named, nameErr := os.Lstat(path)
	if scanErr != nil || statErr != nil || nameErr != nil || !named.Mode().IsRegular() || !os.SameFile(info, actual) || !os.SameFile(actual, named) || actual.Size() != info.Size() || !actual.ModTime().Equal(info.ModTime()) {
		return errors.Join(ErrProjectionSource, scanErr, statErr, nameErr)
	}
	end, err := decodeChainHash(segment.EndHash)
	if err != nil || partial || head.Offset != segment.Length || head.Sequence != segment.LastSequence || end != v.hash || segmentEndEpoch(segment) != v.epoch {
		return errors.Join(ErrTampered, err)
	}
	return s.ctx.Err()
}
