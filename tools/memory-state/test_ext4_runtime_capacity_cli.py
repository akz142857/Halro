import json
import pathlib
import sys
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

import ext4_runtime_capacity_linux as driver


class Ext4RuntimeCapacityCLITest(unittest.TestCase):
    def test_real_command_space_gate_precedes_docker_and_image_creation(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as parent:
            output = pathlib.Path(parent) / 'new-evidence'
            argv = ['ext4_runtime_capacity_linux.py', '--binary', sys.executable,
                    '--output', str(output), '--requests', '1',
                    '--host-backing-directory', parent, '--image-mib', '32768']
            with mock.patch.object(sys, 'argv', argv), mock.patch.object(driver, 'fingerprints', return_value={}), \
                 mock.patch('host_backing.os.statvfs', return_value=SimpleNamespace(
                     f_bavail=2048, f_frsize=4096, f_blocks=4096)), \
                 mock.patch.object(driver.subprocess, 'run', side_effect=AssertionError('Docker before host qualification')):
                with self.assertRaisesRegex(ValueError, 'lacks image'):
                    driver.main()
            evidence = json.loads((output / 'verification.json').read_text())
            self.assertEqual(evidence['result'], 'failed')
            self.assertTrue(evidence['source_unchanged'])
            self.assertFalse((output / 'commands.json').exists())
            self.assertEqual(list(pathlib.Path(parent).iterdir()), [output])
            self.assertEqual(json.loads((output / 'host-backing.json').read_text())['state'], 'refused_before_create')

    def test_image_flag_bounds_before_binary_output_or_docker(self):
        for mib in ('127', '32769', 'not-integer'):
            argv = ['ext4_runtime_capacity_linux.py', '--binary', '/nonexistent',
                    '--output', '/nonexistent', '--requests', '1', '--image-mib', mib]
            with mock.patch.object(sys, 'argv', argv), mock.patch.object(driver.subprocess, 'run',
                       side_effect=AssertionError('invalid option reached Docker')):
                with self.assertRaises(SystemExit) as result:
                    driver.main()
                self.assertEqual(result.exception.code, 2)


if __name__ == '__main__':
    unittest.main()
