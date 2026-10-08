//go:build darwin || linux

package lock

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/akz142857/Halro/internal/ownedfile"
)

var ErrAlreadyLocked = errors.New("data directory is already locked by another process")

type Lock struct {
	mu           sync.Mutex
	file         *os.File
	closed       bool
	closeErr     error
	closing      bool
	closeDone    chan struct{}
	cleanup      [8]func() error
	cleanupCount int
}

// RetainCleanup binds a finite actual descendant lifetime to this process
// exclusion. Close withdraws new ownership and joins every registered cleanup
// before releasing flock. Any uncertain cleanup preserves the lock and owner.
func (l *Lock) RetainCleanup(cleanup func() error) error {
	if l == nil || cleanup == nil {
		return errors.New("missing directory cleanup owner")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.closing || l.file == nil || l.cleanupCount == len(l.cleanup) {
		return errors.New("directory cleanup owner admission is closed or full")
	}
	l.cleanup[l.cleanupCount] = cleanup
	l.cleanupCount++
	return nil
}

func Acquire(dataDir string) (*Lock, error) {
	return acquireDataDirectory(dataDir, func(path string) (*Lock, error) { return acquireFile(publicationLockPath(path), "publication") })
}

// Tests change only the actual publication descriptor lifetime.
func acquireDataDirectory(dataDir string, acquirePublication func(string) (*Lock, error)) (result *Lock, resultErr error) {
	publication, err := acquirePublication(dataDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if e := publication.Close(); e != nil {
			resultErr = errors.Join(resultErr, e)
			if result != nil {
				resultErr = errors.Join(resultErr, result.Close())
				result = nil
			}
		}
	}()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	path := filepath.Join(dataDir, ".halro.lock")
	file, err := openLockFile(path, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, fmt.Errorf("open data lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := ownedfile.CloseFile(file)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(ErrAlreadyLocked, closeErr)
		}
		return nil, errors.Join(fmt.Errorf("acquire data lock: %w", err), closeErr)
	}
	if err = file.Truncate(0); err == nil {
		if _, err = fmt.Fprintf(file, "%d\n", os.Getpid()); err == nil {
			err = file.Sync()
		}
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("write data lock: %w", err), syscall.Flock(int(file.Fd()), syscall.LOCK_UN), ownedfile.CloseFile(file))
	}
	return &Lock{file: file}, nil
}

// AcquireInitialization serializes creation and atomic publication of a new
// data directory without creating that directory first. Normal writers briefly
// take the same sibling guard before opening .halro.lock, so they cannot
// race the final directory rename.
func AcquireInitialization(dataDir string) (*Lock, error) {
	return acquireFile(publicationLockPath(dataDir), "initialization publication")
}

func publicationLockPath(dataDir string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(dataDir)))
	return filepath.Join(filepath.Dir(dataDir), fmt.Sprintf(".halro-publication-%x.lock", digest[:8]))
}

// PublicationPath identifies the sibling guard used by initialization and
// normal writers. Derived-file owners use it to reject native file aliases.
func PublicationPath(dataDir string) string {
	return publicationLockPath(dataDir)
}

func acquireFile(path, purpose string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create %s lock directory: %w", purpose, err)
	}
	file, err := openLockFile(path, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, fmt.Errorf("open %s lock: %w", purpose, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := ownedfile.CloseFile(file)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(ErrAlreadyLocked, closeErr)
		}
		return nil, errors.Join(fmt.Errorf("acquire %s lock: %w", purpose, err), closeErr)
	}
	return &Lock{file: file}, nil
}

// AcquireExistingReadOnly coordinates with the normal writer lock without
// creating, truncating, or rewriting the lock file. Initialized data
// directories always contain this file.
func AcquireExistingReadOnly(dataDir string) (*Lock, error) {
	path := filepath.Join(dataDir, ".halro.lock")
	file, err := openLockFile(path, os.O_RDONLY)
	if err != nil {
		return nil, fmt.Errorf("open existing data lock read-only: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := ownedfile.CloseFile(file)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(ErrAlreadyLocked, closeErr)
		}
		return nil, errors.Join(fmt.Errorf("acquire existing data lock read-only: %w", err), closeErr)
	}
	return &Lock{file: file}, nil
}

func (l *Lock) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if l.closing {
		done := l.closeDone
		l.mu.Unlock()
		<-done
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.closeErr
	}
	if l.closed {
		defer l.mu.Unlock()
		return l.closeErr
	}
	l.closing, l.closeDone = true, make(chan struct{})
	owners, count := l.cleanup, l.cleanupCount
	l.mu.Unlock()
	var cleanupErr error
	for i := count - 1; i >= 0; i-- {
		cleanupErr = errors.Join(cleanupErr, owners[i]())
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	defer close(l.closeDone)
	if cleanupErr != nil {
		l.closeErr = errors.Join(ownedfile.ErrRetirement, cleanupErr)
		return l.closeErr // Original exclusion and all cleanup owners stay reachable.
	}
	l.closed = true
	if l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := ownedfile.CloseFile(l.file)
	if closeErr == nil {
		l.file = nil
	}
	l.closeErr = errors.Join(unlockErr, closeErr)
	return l.closeErr
}

// CoversDataDirectory reports whether l is the live exclusive lock opened for
// dataDir. Derived-state publishers use this to require the same process-wide
// serialization as native writers before replacing a durable root.
func (l *Lock) CoversDataDirectory(dataDir string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.closing || l.file == nil {
		return false
	}
	want := filepath.Join(filepath.Clean(dataDir), ".halro.lock")
	if filepath.Clean(l.file.Name()) != want {
		return false
	}
	descriptor, err := l.file.Stat()
	if err != nil || !descriptor.Mode().IsRegular() {
		return false
	}
	named, err := os.Lstat(want)
	return err == nil && os.SameFile(descriptor, named)
}

// CoversNativePath extends the same process-wide exclusion to regular native
// files in real descendant directories (audit/, governance/, memory-state/).
// It never grants another directory's lock, follows symlink ancestors or accepts
// traversal outside the locked tree. Native readers still check their own inode.
func (l *Lock) CoversNativePath(path string) bool {
	if l == nil || !filepath.IsAbs(path) {
		return false
	}
	l.mu.Lock()
	if l.closed || l.closing || l.file == nil {
		l.mu.Unlock()
		return false
	}
	root := filepath.Dir(l.file.Name())
	l.mu.Unlock()
	if !l.CoversDataDirectory(root) {
		return false
	}
	parent := filepath.Dir(filepath.Clean(path))
	rel, err := filepath.Rel(root, parent)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) > 32 {
		return false
	}
	current := root
	for _, part := range parts {
		if part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return l.CoversDataDirectory(root)
}

// A lock must not rewrite or wait on a foreign nonregular/symlink/alias file.
// The descriptor and name are checked before flock or owner metadata writes.
func openLockFile(path string, flags int) (*os.File, error) {
	f, err := os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	named, nameErr := os.Lstat(path)
	if err != nil || nameErr != nil || !info.Mode().IsRegular() || !named.Mode().IsRegular() || !os.SameFile(info, named) {
		return nil, errors.Join(errors.New("lock descriptor and regular pathname do not match"), err, nameErr, ownedfile.CloseFile(f))
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink != 1 {
		return nil, errors.Join(errors.New("lock file has an unqualified physical alias"), ownedfile.CloseFile(f))
	}
	return f, nil
}
