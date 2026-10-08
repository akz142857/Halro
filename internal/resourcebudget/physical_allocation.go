package resourcebudget

import (
	"errors"
	"os"
)

var ErrPhysicalAllocation = errors.New("physical completion allocation unavailable")
var ErrPhysicalAllocationUncertain = errors.New("physical allocation operation did not join")

// ErrPhysicalAllocationNoWrite proves that the allocation syscall was not run.
// It is not a statement about an ENOSPC returned by an attempted allocation.
var ErrPhysicalAllocationNoWrite = errors.New("physical allocation refused before mutation")

// AllocatePhysicalRange reserves blocks on the actual future write descriptor.
// It leaves EOF unchanged. Success is not permission to trust file contents,
// and cannot guarantee success against later filesystem/IO failures.
func AllocatePhysicalRange(file *os.File, start, bytes int64) error {
	if file == nil || start < 0 || bytes <= 0 || start > 1<<50 || bytes > (1<<50)-start {
		return errors.Join(ErrPhysicalAllocation, ErrInvalid)
	}
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return errors.Join(ErrPhysicalAllocation, err)
	}
	if err := allocatePhysicalRange(file, start, bytes); err != nil {
		return errors.Join(ErrPhysicalAllocation, err)
	}
	if err := file.Sync(); err != nil {
		return errors.Join(ErrPhysicalAllocation, ErrPhysicalAllocationUncertain, err)
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() {
		return errors.Join(ErrPhysicalAllocation, ErrPhysicalAllocationUncertain, err)
	}
	return nil
}

// RetirePhysicalTail releases only the unused allocated tail of this actual
// inode. The caller must independently own its identity and allocated end.
// EOF is checked before and after the operation; committed bytes are untouched.
func RetirePhysicalTail(file *os.File, eof, allocatedEnd int64) error {
	if file == nil || eof < 0 || allocatedEnd <= eof || allocatedEnd > 1<<50 {
		return errors.Join(ErrPhysicalAllocation, ErrInvalid)
	}
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() != eof {
		return errors.Join(ErrPhysicalAllocation, err)
	}
	if err := retirePhysicalTail(file, eof, allocatedEnd); err != nil {
		return errors.Join(ErrPhysicalAllocation, err)
	}
	if err := file.Sync(); err != nil {
		return errors.Join(ErrPhysicalAllocation, err)
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || after.Size() != eof {
		return errors.Join(ErrPhysicalAllocation, err)
	}
	return nil
}
