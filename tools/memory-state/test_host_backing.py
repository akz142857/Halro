import json
import os
import pathlib
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

from host_backing import HostBacking

CLEANUP = {'backing': '/halro-host-owned/state.ext4.img', 'unmounted': True,
           'loop_absent': True, 'autoclear_completed': True, 'reader_closed': True}


class HostBackingSafetyTest(unittest.TestCase):
    def test_space_refusal_has_no_namespace_or_allocation(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as parent:
            evidence = pathlib.Path(parent) / 'evidence.json'
            with mock.patch('host_backing.os.statvfs', return_value=SimpleNamespace(
                    f_bavail=1024, f_frsize=4096, f_blocks=2048)), mock.patch(
                    'host_backing.tempfile.mkdtemp', side_effect=AssertionError('created before space check')):
                with self.assertRaisesRegex(ValueError, 'lacks image'):
                    HostBacking(parent, 512, evidence)
            self.assertEqual(json.loads(evidence.read_text())['state'], 'refused_before_create')
            self.assertEqual(list(pathlib.Path(parent).iterdir()), [evidence])

    def test_bounds_and_non_tmp_directory_refuse_before_create(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as parent:
            evidence = pathlib.Path(parent) / 'evidence.json'
            for mib in (127, 32769, True):
                with self.assertRaises(ValueError):
                    HostBacking(parent, mib, evidence)
            with self.assertRaisesRegex(ValueError, 'under /tmp'):
                HostBacking('/', 512, evidence)
            self.assertEqual(list(pathlib.Path(parent).iterdir()), [])

    def fixture(self, parent):
        # Small actual creator/FD/parent objects exercise retirement. This is
        # not evidence that a sparse 512MiB image is physically allocated.
        o = HostBacking.__new__(HostBacking)
        o.directory = pathlib.Path(parent) / 'owned'
        o.directory.mkdir(mode=0o700)
        o.parent_fd = os.open(o.directory, os.O_RDONLY | os.O_DIRECTORY)
        o.parent = os.fstat(o.parent_fd)
        o.path = o.directory / 'state.ext4.img'
        o.fd = os.open(o.path, os.O_RDWR | os.O_CREAT | os.O_EXCL, 0o600)
        self.addCleanup(lambda: os.close(o.fd) if o.fd is not None else None)
        self.addCleanup(lambda: os.close(o.parent_fd) if o.parent_fd is not None else None)
        os.write(o.fd, b'x' * 8192)
        os.fsync(o.fd)
        o.size, o.birth = 8192, os.fstat(o.fd)
        o.record, o.evidence = {}, pathlib.Path(parent) / 'evidence.json'
        return o

    def test_cleanup_unknown_or_integer_claim_preserves_original_owner(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as parent:
            o = self.fixture(parent)
            for proof in (None, {}, {**CLEANUP, 'reader_closed': 1}):
                with self.assertRaisesRegex(ValueError, 'unconfirmed'):
                    o.retire(proof)
                self.assertEqual(os.fstat(o.fd).st_ino, o.birth.st_ino)
                self.assertEqual(o.path.read_bytes(), b'x' * 8192)

    def test_foreign_replacement_not_unlinked(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as parent:
            o = self.fixture(parent)
            o.path.rename(o.directory / 'original')
            o.path.write_bytes(b'foreign')
            with self.assertRaisesRegex(ValueError, 'changed'):
                o.retire(CLEANUP)
            self.assertEqual(o.path.read_bytes(), b'foreign')
            self.assertEqual(os.fstat(o.fd).st_ino, o.birth.st_ino)

    def test_unknown_actual_close_preserves_image(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as parent:
            o = self.fixture(parent)
            with mock.patch('host_backing.os.close', side_effect=OSError('unknown Close')):
                with self.assertRaisesRegex(OSError, 'unknown Close'):
                    o.retire(CLEANUP)
            self.assertTrue(o.path.exists())
            self.assertEqual(os.fstat(o.fd).st_ino, o.birth.st_ino)

    def test_only_actual_joined_same_creator_is_retired(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as parent:
            o = self.fixture(parent)
            o.retire(CLEANUP)
            self.assertFalse(o.path.exists())
            self.assertFalse(o.directory.exists())
            self.assertEqual(o.record['state'], 'retired_after_actual_loop_cleanup')


if __name__ == '__main__':
    unittest.main()
