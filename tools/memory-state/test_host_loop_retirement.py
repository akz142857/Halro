import json
import os
import pathlib
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

from host_loop_retirement import finish_host_join, prepared_join, PREPARE_MARKER, JOIN_MARKER
import test_host_backing


class HostLoopRetirementSafetyTest(unittest.TestCase):
    def fixture(self, parent):
        return test_host_backing.HostBackingSafetyTest.fixture(self, parent)

    def receipt(self, size):
        return {'backing': '/halro-host-owned/state.ext4.img', 'unmounted': True,
                'detach_requested': True, 'reader_closed': True, 'backing_device': '0:36',
                'backing_inode': 991, 'backing_size': size, 'original_loop': '/dev/loop2',
                'original_loop_device': '7:2', 'namespace_join_pending': True}

    def log(self, receipt):
        return PREPARE_MARKER + json.dumps(receipt) + '\n'

    def proof(self, receipt, **changes):
        result = {key: receipt[key] for key in ('backing_device', 'backing_inode',
                  'backing_size', 'original_loop', 'original_loop_device')}
        result.update(loop_absent=True, observer_reader_closed=True, polls=0)
        result.update(changes)
        return SimpleNamespace(returncode=0, stdout=JOIN_MARKER + json.dumps(result) + '\n')

    def test_stopped_or_exit_code_alone_never_admits_retirement(self):
        receipt = self.receipt(8192)
        for state in (None, {'Running': True, 'Pid': 0}, {'Running': False, 'Pid': 77},
                      {'Running': False, 'Pid': False}, {'Running': False, 'ExitCode': 0}):
            with self.assertRaisesRegex(ValueError, 'namespace not joined'):
                prepared_join(self.log(receipt), state, 8192)
        for bad in ({**receipt, 'reader_closed': 1}, {**receipt, 'backing_size': 8193},
                    {**receipt, 'original_loop_device': '7:3'},
                    {**receipt, 'unmounted': False}, {**receipt, 'additional': True}):
            with self.assertRaises(ValueError):
                prepared_join(self.log(bad), {'Running': False, 'Pid': 0}, 8192)
        with self.assertRaisesRegex(ValueError, 'unique'):
            prepared_join(self.log(receipt) * 2, {'Running': False, 'Pid': 0}, 8192)

    def test_unknown_namespace_remove_or_wrong_independent_inode_retains_actual_creator(self):
        for phase in ('namespace', 'identity', 'reader'):
            with self.subTest(phase=phase), tempfile.TemporaryDirectory(dir='/tmp') as parent:
                host = self.fixture(parent)
                receipt = self.receipt(host.size)
                def docker(*args):
                    self.assertEqual(os.fstat(host.fd).st_ino, host.birth.st_ino)
                    if args[0] == 'rm':
                        if phase == 'namespace':
                            raise OSError('unknown namespace join')
                        return SimpleNamespace(returncode=0, stdout='owned\n')
                    return self.proof(receipt, **({'backing_inode': 992} if phase == 'identity'
                                                 else {'observer_reader_closed': 1}))
                with self.assertRaises((OSError, ValueError)):
                    finish_host_join(host, {'Running': False, 'Pid': 0}, self.log(receipt),
                                     docker, 'image', 'owned', pathlib.Path(parent))
                self.assertEqual(os.fstat(host.fd).st_ino, host.birth.st_ino)
                self.assertEqual(host.path.read_bytes(), b'x' * 8192)

    def test_replacement_during_fresh_observer_preserves_foreign_and_original_fd(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as parent:
            host = self.fixture(parent)
            receipt = self.receipt(host.size)
            def docker(*args):
                if args[0] == 'run':
                    host.path.rename(host.directory / 'original')
                    host.path.write_bytes(b'foreign')
                    return self.proof(receipt)
                return SimpleNamespace(returncode=0, stdout='owned\n')
            with self.assertRaisesRegex(ValueError, 'changed'):
                finish_host_join(host, {'Running': False, 'Pid': 0}, self.log(receipt),
                                 docker, 'image', 'owned', pathlib.Path(parent))
            self.assertEqual(host.path.read_bytes(), b'foreign')
            self.assertEqual(os.fstat(host.fd).st_ino, host.birth.st_ino)

    def test_actual_creator_stays_open_until_all_namespace_and_reader_joins(self):
        # This unit seam covers lifecycle ordering, not actual kernel/FS proof.
        # The separately opt-in ext4 test supplies that independent evidence.
        with tempfile.TemporaryDirectory(dir='/tmp') as parent:
            host = self.fixture(parent)
            receipt = self.receipt(host.size)
            calls = []
            def docker(*args):
                calls.append(args[0])
                self.assertEqual(os.fstat(host.fd).st_ino, host.birth.st_ino)
                return self.proof(receipt) if args[0] == 'run' else SimpleNamespace(returncode=0, stdout='owned\n')
            finish_host_join(host, {'Running': False, 'Pid': 0}, self.log(receipt),
                             docker, 'image', 'owned', pathlib.Path(parent))
            self.assertEqual(calls, ['rm', 'run'])
            self.assertIsNone(host.fd)
            self.assertIsNone(host.parent_fd)
            self.assertFalse(host.path.exists())
            self.assertTrue(host.record['independent_loop_absent_verified'])

    def test_unknown_actual_host_close_keeps_image_after_true_join(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as parent:
            host = self.fixture(parent)
            receipt = self.receipt(host.size)
            def docker(*args):
                return self.proof(receipt) if args[0] == 'run' else SimpleNamespace(returncode=0, stdout='owned\n')
            with mock.patch('host_backing.os.close', side_effect=OSError('unknown actual Close')):
                with self.assertRaisesRegex(OSError, 'unknown actual Close'):
                    finish_host_join(host, {'Running': False, 'Pid': 0}, self.log(receipt),
                                     docker, 'image', 'owned', pathlib.Path(parent))
            self.assertEqual(os.fstat(host.fd).st_ino, host.birth.st_ino)
            self.assertTrue(host.path.exists())


if __name__ == '__main__':
    unittest.main()
