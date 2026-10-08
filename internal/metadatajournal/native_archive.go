package metadatajournal

import (
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/store/lock"
)

// NativeArchive preserves original bytes. It captures the real authenticated
// journal descriptor and never manufactures a deletion or refund receipt.
type NativeArchive struct {
	mu                  sync.Mutex
	log                 *Log
	guard               *lock.Lock
	head                Head
	file, parent        os.FileInfo
	source, destination string
	moved               bool
	closeErr            error
}

func (l *Log) PrepareNativeArchive(expected Head, destination string, guard *lock.Lock) (*NativeArchive, error) {
	if l == nil || guard == nil || !filepath.IsAbs(destination) || !guard.CoversNativePath(l.path) || !guard.CoversNativePath(destination) {
		return nil, ErrCorrupt
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	actual := Head{Epoch: l.epoch, Sequence: l.sequence, Offset: l.offset, Hash: l.lastHash, TrimmedThrough: l.trimmedThrough, FormatVersion: l.formatVersion, FileRevision: l.fileRevision, Baseline: l.baseline}
	if l.file == nil || l.terminalErr != nil || actual != expected {
		return nil, errors.Join(ErrCorrupt, l.terminalErr)
	}
	file, err := l.file.Stat()
	if err != nil {
		return nil, err
	}
	current, err := os.Lstat(l.path)
	if err != nil || !os.SameFile(current, file) || !file.Mode().IsRegular() || file.Mode().Perm()&0077 != 0 {
		return nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	parent, err := os.Lstat(filepath.Dir(l.path))
	if err != nil || !parent.IsDir() {
		return nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	return &NativeArchive{log: l, guard: guard, head: expected, file: file, parent: parent, source: l.path, destination: destination}, nil
}
func (a *NativeArchive) Archive() error {
	if a == nil {
		return ErrCorrupt
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closeErr != nil {
		return a.closeErr
	}
	if a.moved {
		return nil
	}
	if a.guard == nil || !a.guard.CoversNativePath(a.source) || !a.guard.CoversNativePath(a.destination) {
		return ownedfile.ErrReplaced
	}
	if err := a.log.Close(); err != nil {
		a.closeErr = err
		return err
	}
	parent, err := os.Lstat(filepath.Dir(a.source))
	if err != nil || !os.SameFile(parent, a.parent) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	current, err := os.Lstat(a.source)
	if err != nil || !os.SameFile(current, a.file) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	if _, err := os.Lstat(a.destination); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	if err := os.Rename(a.source, a.destination); err != nil {
		return err
	}
	a.moved = true
	err = errors.Join(durable.SyncDirectory(filepath.Dir(a.source)), durable.SyncDirectory(filepath.Dir(a.destination)))
	if err != nil {
		a.closeErr = errors.Join(ownedfile.ErrRetirement, err)
	}
	return a.closeErr
}
