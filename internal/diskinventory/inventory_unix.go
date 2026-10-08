//go:build linux || darwin

package diskinventory

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

type scan struct {
	ctx       context.Context
	opts      Options
	seen      Snapshot
	device    uint64
	closeFile func(*os.File) error
	closeErr  error
}

func reconcile(ctx context.Context, opts Options, closeFile func(*os.File) error) (result *Baseline, resultErr error) {
	if ctx == nil || opts.DataDir == "" || opts.Budget == nil || opts.Guard == nil || opts.Ceiling <= 0 || opts.Ceiling > 1<<50 || opts.Deadline <= 0 || opts.Deadline > time.Hour {
		return nil, ErrNamespace
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := filepath.Abs(opts.DataDir)
	if err != nil || !opts.Guard.CoversDataDirectory(root) {
		return nil, errors.Join(ErrNamespace, err)
	}
	opts.DataDir = root
	credit, err := opts.Budget.TryAcquireRecovery(resourcebudget.Cost{RAM: WorkspaceRAMBytes, Disk: opts.Ceiling})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Deadline)
	defer cancel()
	s := scan{ctx: ctx, opts: opts, closeFile: closeFile}
	var rootIdentity os.FileInfo
	if s.closeFile == nil {
		s.closeFile = ownedfile.CloseFile
	}
	defer func() {
		if s.closeErr != nil {
			result = &Baseline{credit: credit, cleanupErr: errors.Join(ErrRetirement, s.closeErr)}
			resultErr = errors.Join(resultErr, result.cleanupErr)
			return
		}
		if resultErr == nil {
			resultErr = ctx.Err()
			if resultErr == nil {
				named, err := os.Lstat(root)
				if err != nil || rootIdentity == nil || !named.IsDir() || !os.SameFile(rootIdentity, named) || rootIdentity.Size() != named.Size() || !rootIdentity.ModTime().Equal(named.ModTime()) || !opts.Guard.CoversDataDirectory(root) {
					resultErr = errors.Join(ErrNamespace, err)
				}
			}
		}
		if resultErr != nil {
			resultErr = errors.Join(resultErr, credit.Release())
			return
		}
		unused := resourcebudget.Cost{RAM: WorkspaceRAMBytes, Disk: opts.Ceiling - s.seen.ChargeBytes}
		if err := credit.ReleasePart(unused); err != nil {
			result = &Baseline{credit: credit, cleanupErr: err}
			resultErr = err
			return
		}
		result = &Baseline{observation: s.seen, credit: credit}
	}()
	named, err := os.Lstat(root)
	if err != nil || !named.IsDir() {
		return nil, errors.Join(ErrNamespace, err)
	}
	f, err := os.OpenFile(root, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer s.close(f)
	actual, err := f.Stat()
	if err != nil || !os.SameFile(named, actual) {
		return nil, errors.Join(ErrNamespace, err)
	}
	rootIdentity = actual
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return nil, err
	}
	s.device = uint64(st.Dev)
	if err := s.directory(f, actual, 0); err != nil {
		return nil, err
	}
	final, err := os.Lstat(root)
	if err != nil || !os.SameFile(actual, final) || !opts.Guard.CoversDataDirectory(root) {
		return nil, errors.Join(ErrNamespace, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, nil // the defer publishes only after all actual descriptors close
}

func (s *scan) close(f *os.File) {
	if err := s.closeFile(f); err != nil {
		// Preserve the actual descriptor even when an injected close seam merely
		// reports an error. A pathname cannot retain this IO obligation.
		s.closeErr = errors.Join(s.closeErr, ownedfile.CloseError(f, err))
	}
}

func (s *scan) account(st unix.Stat_t, directory bool) error {
	if st.Size < 0 || st.Blocks < 0 || st.Blocks > math.MaxInt64/512 || uint64(st.Dev) != s.device {
		return ErrNamespace
	}
	allocated := st.Blocks * 512
	logical := int64(0)
	charge := allocated
	if !directory {
		if st.Nlink != 1 {
			return ErrNamespace
		}
		logical = st.Size
		if logical > charge {
			charge = logical
		}
	}
	if logical > math.MaxInt64-s.seen.LogicalBytes || allocated > math.MaxInt64-s.seen.AllocatedBytes || charge > s.opts.Ceiling-s.seen.ChargeBytes {
		return ErrLimit
	}
	if directory {
		if s.seen.Directories == math.MaxUint64 {
			return ErrLimit
		}
		s.seen.Directories++
	} else {
		if s.seen.Files == math.MaxUint64 {
			return ErrLimit
		}
		s.seen.Files++
	}
	s.seen.LogicalBytes += logical
	s.seen.AllocatedBytes += allocated
	s.seen.ChargeBytes += charge
	return nil
}

func (s *scan) directory(f *os.File, original os.FileInfo, depth int) error {
	if depth >= MaxDepth {
		return ErrLimit
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrNamespace
	}
	if err := s.account(st, true); err != nil {
		return err
	}
	for {
		if s.closeErr != nil {
			return errors.Join(ErrRetirement, s.closeErr)
		}
		if err := s.ctx.Err(); err != nil {
			return err
		}
		if !s.opts.Guard.CoversDataDirectory(s.opts.DataDir) {
			return ErrNamespace
		}
		names, err := f.Readdirnames(1)
		if len(names) > 1 {
			return ErrNamespace
		}
		if len(names) == 1 {
			if e := s.entry(f, names[0], depth); e != nil {
				return e
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	final, err := f.Stat()
	if err != nil || !os.SameFile(original, final) || original.Size() != final.Size() || !original.ModTime().Equal(final.ModTime()) {
		return errors.Join(ErrNamespace, err)
	}
	return nil
}

func (s *scan) entry(parent *os.File, name string, depth int) (resultErr error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return ErrNamespace
	}
	var before unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG && before.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrNamespace
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	defer s.close(f)
	var actual unix.Stat_t
	if err := unix.Fstat(fd, &actual); err != nil {
		return err
	}
	if !samePhysical(before, actual) {
		return ErrNamespace
	}
	if actual.Mode&unix.S_IFMT == unix.S_IFDIR {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if err := s.directory(f, info, depth+1); err != nil {
			return err
		}
	} else if err := s.account(actual, false); err != nil {
		return err
	}
	var after, descriptor unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &after, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if err := unix.Fstat(fd, &descriptor); err != nil {
		return err
	}
	if !samePhysical(actual, after) || !samePhysical(actual, descriptor) {
		return ErrNamespace
	}
	return s.ctx.Err()
}

func samePhysical(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode && a.Size == b.Size && a.Blocks == b.Blocks && a.Nlink == b.Nlink
}
