"""Exclusive, host-owned backing files for disposable capacity experiments."""
import fcntl
import json
import os
import pathlib
import stat
import struct
import sys
import tempfile


class HostBackingFailure(RuntimeError):
    def __init__(self, owner, cause):
        self.owner = owner
        super().__init__(str(cause))


class HostBacking:
    reserve_bytes = 256 << 20

    def __init__(self, directory, image_mib, evidence):
        if type(image_mib) is not int or not 128 <= image_mib <= 32768:
            raise ValueError("128..32768 image MiB required")
        base = pathlib.Path(directory).resolve(strict=True)
        tmp = pathlib.Path('/tmp').resolve(strict=True)
        if base != tmp and tmp not in base.parents:
            raise ValueError("host backing directory must be an existing directory under /tmp")
        if not base.is_dir():
            raise ValueError("host backing parent is not a directory")
        if any(c in str(base) for c in (',', '\n', '\r')):
            raise ValueError("host backing path cannot contain Docker mount separators")
        self.size = image_mib << 20
        vfs = os.statvfs(base)
        self.evidence = pathlib.Path(evidence)
        self.record = {'host_directory': str(base), 'image_bytes': self.size,
                       'host_available_bytes': vfs.f_bavail * vfs.f_frsize,
                       'host_total_bytes': vfs.f_blocks * vfs.f_frsize,
                       'minimum_remaining_bytes': self.reserve_bytes,
                       'state': 'preflight'}
        self._save()
        if self.record['host_available_bytes'] < self.size + self.reserve_bytes:
            self.record['state'] = 'refused_before_create'
            self._save()
            raise ValueError("host filesystem lacks image bytes plus 256MiB headroom; no backing file created")
        self.directory = pathlib.Path(tempfile.mkdtemp(prefix='halro-capacity-owned-', dir=base))
        self.parent_fd = os.open(self.directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        self.parent = os.fstat(self.parent_fd)
        self.path = self.directory / 'state.ext4.img'
        self.fd = os.open(self.path, os.O_RDWR | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        self.birth = os.fstat(self.fd)
        self.record.update(path=str(self.path), directory=str(self.directory), state='creator_owned',
                           device=self.birth.st_dev, inode=self.birth.st_ino,
                           parent_device=self.parent.st_dev, parent_inode=self.parent.st_ino)
        self._save()
        try:
            if hasattr(os, 'posix_fallocate'):
                os.posix_fallocate(self.fd, 0, self.size)
                method = 'posix_fallocate'
            elif sys.platform == 'darwin':
                # Darwin fstore_t: allocate all requested bytes at physical EOF.
                fcntl.fcntl(self.fd, 42, struct.pack('=Iiqqq', 4, 3, 0, self.size, 0))
                method = 'darwin_F_PREALLOCATE'
            else:
                raise ValueError("host has no supported physical preallocation API")
            os.ftruncate(self.fd, self.size)
            os.fsync(self.fd)
            os.fsync(self.parent_fd)
            self.verify()
            actual = os.fstat(self.fd)
            self.record.update(state='preallocated', allocation_method=method,
                               actual_size=actual.st_size, actual_st_blocks=actual.st_blocks,
                               allocated_bytes=actual.st_blocks * 512)
            self._save()
        except BaseException as error:
            self.record['state'] = 'allocation_failed_retained'
            self._save()
            raise HostBackingFailure(self, error) from error

    def _save(self):
        self.evidence.write_text(json.dumps(self.record, indent=2) + '\n')

    def verify(self):
        parent = os.fstat(self.parent_fd)
        parent_name = os.lstat(self.directory)
        actual = os.fstat(self.fd)
        named = os.stat('state.ext4.img', dir_fd=self.parent_fd, follow_symlinks=False)
        if ((parent.st_dev, parent.st_ino) != (self.parent.st_dev, self.parent.st_ino) or
            (parent_name.st_dev, parent_name.st_ino) != (self.parent.st_dev, self.parent.st_ino) or
            not stat.S_ISDIR(parent_name.st_mode) or
            (actual.st_dev, actual.st_ino) != (self.birth.st_dev, self.birth.st_ino) or
            (named.st_dev, named.st_ino) != (actual.st_dev, actual.st_ino) or
            not stat.S_ISREG(named.st_mode) or named.st_nlink != 1 or
            actual.st_size != self.size or actual.st_blocks * 512 < self.size):
            raise ValueError("actual host FD/name/parent/preallocated extent changed; owner retained")

    def retire(self, cleanup):
        # No process exit or docker rm alone proves loop/FD retirement.
        expected = {'backing': '/halro-host-owned/state.ext4.img',
                    'unmounted': True, 'loop_absent': True,
                    'autoclear_completed': True, 'reader_closed': True}
        if cleanup != expected or any(cleanup.get(k) is not True for k in expected if k != 'backing'):
            self.record['state'] = 'cleanup_unconfirmed_retained'
            self._save()
            raise ValueError("actual ext4/loop/reader cleanup unconfirmed; host owner retained")
        self.verify()
        os.close(self.fd)
        self.fd = None
        # Compare the original creator inode once more after its actual Close.
        named = os.stat('state.ext4.img', dir_fd=self.parent_fd, follow_symlinks=False)
        if (named.st_dev, named.st_ino) != (self.birth.st_dev, self.birth.st_ino):
            raise ValueError("foreign host image preserved after creator Close")
        os.unlink('state.ext4.img', dir_fd=self.parent_fd)
        os.fsync(self.parent_fd)
        parent_name = os.lstat(self.directory)
        if (parent_name.st_dev, parent_name.st_ino) != (self.parent.st_dev, self.parent.st_ino):
            raise ValueError("foreign host parent preserved after image retirement")
        os.close(self.parent_fd)
        self.parent_fd = None
        os.rmdir(self.directory)
        self.record['state'] = 'retired_after_actual_loop_cleanup'
        self._save()
