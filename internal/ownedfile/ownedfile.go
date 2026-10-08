// Package ownedfile records the identity of an exclusively created derivative.
// It grants no authority to reopen saved data or delete unrelated files.
package ownedfile

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var ErrReplaced = errors.New("owned file path was replaced; refusing removal")
var ErrRetirement = errors.New("owned file actual IO retirement is uncertain")

type Owner struct {
	path          string
	file          os.FileInfo
	parent        os.FileInfo
	retirementErr error
	retirementMu  sync.Mutex
	retired       bool
}

// Identity comes only from the exclusive creation descriptor. It is suitable
// for checking another physical reader, not for manufacturing a new owner.
func (o *Owner) Identity() os.FileInfo {
	if o == nil {
		return nil
	}
	o.retirementMu.Lock()
	defer o.retirementMu.Unlock()
	return o.file
}

// Retire records a successful unlink of this receipt's inode.
// a missing original name cannot establish retirement: the inode may have been
// renamed and still consume space. Only a previous successful Retire makes a
// repeated call idempotent. Callers join physical readers and Close first and
// provide directory exclusion; this receipt does not prove freed disk blocks.
func (o *Owner) Retire() error { return o.retireWithPhase(nil) }

func (o *Owner) retireWithPhase(phase func(*os.File, *os.File) error) error {
	if o == nil || o.file == nil {
		return nil
	}
	o.retirementMu.Lock()
	defer o.retirementMu.Unlock()
	if o.retired || o.retirementErr != nil {
		return o.retirementErr
	}
	retired, err := retirePinned(o.path, o.file, o.parent, phase)
	o.retired = retired
	if errors.Is(err, ErrRetirement) {
		o.retirementErr = err
	}
	return err
}

// Create is suitable for a storage engine's OpenFile callback. Identity comes
// from the opened descriptor, not a later path lookup. An existing path is
// refused, including a symlink. The caller owns all physical IO and Close.
func Create(path string, flags int, mode os.FileMode) (*os.File, *Owner, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}
	return createPinned(absolute, flags, mode, nil)
}

// Remove is the compatibility spelling of Retire. A missing original name
// cannot prove that its inode stopped consuming space; only a recorded actual
// unlink makes repeated calls idempotent. The caller joins all other readers.
func (o *Owner) Remove() error { return o.Retire() }

// CreateTemporary captures an exclusively created temporary's actual inode.
// It grants ownership only of the file just created, never an existing path.
func CreateTemporary(directory, pattern string) (*os.File, *Owner, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, nil, err
	}
	if strings.ContainsAny(pattern, "/\\") {
		return nil, nil, ErrReplaced
	}
	at := strings.LastIndexByte(pattern, '*')
	prefix, suffix := pattern, ""
	if at >= 0 {
		prefix, suffix = pattern[:at], pattern[at+1:]
	}
	for attempt := 0; attempt < 100; attempt++ {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, nil, err
		}
		file, owner, err := createPinned(filepath.Join(absolute, prefix+hex.EncodeToString(nonce[:])+suffix), os.O_RDWR, 0600, nil)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return file, owner, err
	}
	return nil, nil, os.ErrExist
}

// ReplaceCaptured moves this newly created inode over the independently
// captured destination. Caller exclusion and directory Sync remain required.
// On successful rename the SAME creation receipt follows the actual inode.
func (o *Owner) ReplaceCaptured(destination string, previous os.FileInfo) error {
	if o == nil || o.file == nil || previous == nil {
		return ErrReplaced
	}
	o.retirementMu.Lock()
	defer o.retirementMu.Unlock()
	if o.retired || o.retirementErr != nil {
		return errors.Join(ErrReplaced, o.retirementErr)
	}
	if err := o.checkParent(); err != nil {
		return err
	}
	current, err := os.Lstat(o.path)
	if err != nil || !os.SameFile(o.file, current) {
		return errors.Join(ErrReplaced, err)
	}
	dest, err := os.Lstat(destination)
	if err != nil || !os.SameFile(previous, dest) {
		return errors.Join(ErrReplaced, err)
	}
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	if filepath.Dir(absolute) != filepath.Dir(o.path) {
		return ErrReplaced
	}
	if err := os.Rename(o.path, absolute); err != nil {
		return err
	}
	o.path = absolute
	return nil
}

// VerifyFile checks the actual descriptor and current name against the same
// creation receipt, including after ReplaceCaptured. It grants no new owner.
func (o *Owner) VerifyFile(f *os.File) error {
	if o == nil || o.file == nil || f == nil {
		return ErrReplaced
	}
	o.retirementMu.Lock()
	defer o.retirementMu.Unlock()
	if o.retired || o.retirementErr != nil {
		return errors.Join(ErrReplaced, o.retirementErr)
	}
	if err := o.checkParent(); err != nil {
		return err
	}
	actual, err := f.Stat()
	named, nameErr := os.Lstat(o.path)
	if err != nil || nameErr != nil || !os.SameFile(o.file, actual) || !os.SameFile(o.file, named) {
		return errors.Join(ErrReplaced, err, nameErr)
	}
	return nil
}

// VerifyName checks only the creation receipt's namespace. It neither joins
// physical readers nor proves freed blocks, and grants no new removal owner.
func (o *Owner) VerifyName() error {
	if o == nil || o.file == nil {
		return ErrReplaced
	}
	o.retirementMu.Lock()
	defer o.retirementMu.Unlock()
	if o.retired || o.retirementErr != nil {
		return errors.Join(ErrReplaced, o.retirementErr)
	}
	if err := o.checkParent(); err != nil {
		return err
	}
	named, err := os.Lstat(o.path)
	if err != nil || !os.SameFile(o.file, named) {
		return errors.Join(ErrReplaced, err)
	}
	return nil
}

// LinkCaptured exclusively publishes an alias of this creation receipt in the
// same directory. Both names own separate unlink obligations for the SAME
// inode. A non-nil alias means Link succeeded, even when the final name check
// fails; callers must preserve that uncertain publication and its fees.
// Directory exclusion, physical Close and directory Sync are caller duties.
func (o *Owner) LinkCaptured(destination string) (*Owner, error) {
	if o == nil || o.file == nil {
		return nil, ErrReplaced
	}
	o.retirementMu.Lock()
	defer o.retirementMu.Unlock()
	if o.retired || o.retirementErr != nil {
		return nil, errors.Join(ErrReplaced, o.retirementErr)
	}
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return nil, err
	}
	if filepath.Dir(absolute) != filepath.Dir(o.path) {
		return nil, ErrReplaced
	}
	if err := o.checkParent(); err != nil {
		return nil, err
	}
	current, err := os.Lstat(o.path)
	if err != nil || !os.SameFile(o.file, current) {
		return nil, errors.Join(ErrReplaced, err)
	}
	if err := os.Link(o.path, absolute); err != nil {
		return nil, err
	}
	alias := &Owner{path: absolute, file: o.file, parent: o.parent}
	if err := o.checkParent(); err != nil {
		return alias, errors.Join(ErrRetirement, err)
	}
	named, err := os.Lstat(absolute)
	if err != nil || !os.SameFile(o.file, named) {
		return alias, errors.Join(ErrReplaced, err)
	}
	return alias, nil
}

func (o *Owner) checkParent() error {
	current, err := os.Lstat(filepath.Dir(o.path))
	if err != nil || o.parent == nil || !current.IsDir() || !os.SameFile(o.parent, current) {
		return errors.Join(ErrReplaced, err)
	}
	return nil
}
