package ledger

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
)

// A complete streaming publication source over the pinned old directory.
func (d *segmentDirectoryIndex) source(appendEntry, replacement, pending *Segment) segmentManifestSource {
	return segmentManifestSource{pending: pending, each: func(emit func(Segment) error) error {
		for generation := uint64(1); generation <= d.parsed.count; generation++ {
			segment, found, err := d.Get(generation)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("%w: committed directory generation missing", ErrCorrupt)
			}
			if replacement != nil && replacement.Generation == generation {
				segment = *replacement
			}
			if err := emit(segment); err != nil {
				return err
			}
		}
		if appendEntry != nil {
			return emit(*appendEntry)
		}
		return nil
	}}
}

func openRuntimeSegmentDirectory(directory string, replica bool, maxIndexBytes int64, key []byte) (result *segmentDirectoryIndex, err error) {
	return openRuntimeSegmentDirectoryForStartup(directory, replica, maxIndexBytes, key, nil)
}

func openRuntimeSegmentDirectoryForStartup(directory string, replica bool, maxIndexBytes int64, key []byte, selected *selectedNativeStartup) (result *segmentDirectoryIndex, err error) {
	return openRuntimeSegmentDirectoryForStartupWithDisk(directory, replica, maxIndexBytes, key, selected, nil)
}

func openRuntimeSegmentDirectoryForStartupWithDisk(directory string, replica bool, maxIndexBytes int64, key []byte, selected *selectedNativeStartup, reserve reserveNativeFileDisk) (result *segmentDirectoryIndex, err error) {
	d, err := buildRuntimeSegmentDirectoryIndexWithDisk(directory, maxIndexBytes, key, reserve)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			closeErr := d.Close()
			if closeErr != nil && selected != nil {
				selected.retainedDirectory = d
				selected.retirementErr = errors.Join(selected.retirementErr, closeErr)
			}
			err = errors.Join(err, closeErr)
		}
	}()
	if pending := d.parsed.pending; pending != nil {
		if selected != nil {
			if err = selected.recoverInterruptedRoll(d, directory, maxIndexBytes, key, reserve); err != nil {
				return nil, err
			}
		} else {
			if replica {
				return nil, errors.New("replica ledger has an interrupted Roll that must be reconciled against ordering")
			}
			checksum, length, readErr := hashFile(filepath.Join(directory, pending.File))
			var appendEntry *Segment
			if !errors.Is(readErr, os.ErrNotExist) {
				if readErr != nil {
					return nil, readErr
				}
				if length != pending.Length || checksum != pending.PlainChecksum {
					return nil, fmt.Errorf("%w: interrupted ledger roll left generation %d unreadable", ErrCorrupt, pending.Generation)
				}
				appendEntry = pending
				if err = checkSegmentDirectoryIndexLimit(d.parsed.count+1, maxIndexBytes); err != nil {
					return nil, err
				}
			}
			if _, err = saveSegmentManifestSourceWithDisk(directory, d.source(appendEntry, nil, nil), durable.SyncDirectory, reserve); err != nil {
				return nil, err
			}
		}
		next, buildErr := buildRuntimeSegmentDirectoryIndexWithDisk(directory, maxIndexBytes, key, reserve)
		if buildErr != nil {
			return nil, buildErr
		}
		if closeErr := d.Close(); closeErr != nil {
			next.Close()
			return nil, closeErr
		}
		d = next
	}
	for generation := uint64(1); generation <= d.parsed.count; generation++ {
		segment, found, readErr := d.Get(generation)
		if readErr != nil {
			return nil, readErr
		}
		if !found {
			return nil, ErrCorrupt
		}
		if err = checkSegmentPresent(directory, segment); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// All access occurs under Log.mu. Index/read failure latches native recovery;
// out-of-range caller input is an ordinary missing generation.
func (l *Log) lookupSegmentLocked(generation uint64) (Segment, bool, error) {
	if l.segmentDirectory == nil {
		return Segment{}, false, errors.New("Ledger directory is closed")
	}
	segment, found, err := l.segmentDirectory.Get(generation)
	if err != nil {
		l.status.RequireRecovery()
	}
	return segment, found, err
}
func (l *Log) refreshSegmentDirectoryLocked() error {
	old := l.segmentDirectory
	l.segmentDirectory = nil
	if err := old.Close(); err != nil {
		l.status.RequireRecovery()
		return err
	}
	// Keep the canonical locator until a complete replacement is durable.
	// The builder verifies the new manifest, captures the original old inode,
	// and replaces it atomically. A borrowed locator never grants unlink
	// authority; missing names may conceal a renamed, still-live old inode.
	if old != nil && old.persistent && old.indexInfo != nil {
		if err := old.checkParent(); err != nil {
			l.status.RequireRecovery()
			return err
		}
		named, err := os.Lstat(filepath.Join(l.directory, segmentDirectoryIndexName))
		if err != nil || !named.Mode().IsRegular() || !os.SameFile(old.indexInfo, named) {
			l.status.RequireRecovery()
			return errors.Join(ownedfile.ErrReplaced, err)
		}
	}
	next, err := buildRuntimeSegmentDirectoryIndexWithDisk(l.directory, l.options.MaxSegmentDirectoryIndexBytes, l.chainKey, l.options.ReserveNativeFileDisk)
	if err != nil {
		l.status.RequireRecovery()
		return err
	}
	l.segmentDirectory = next
	return nil
}
