import pathlib
import tempfile
import unittest

from source_fingerprints import fingerprints


class SourceFingerprintsTest(unittest.TestCase):
    def test_actual_shell_runner_python_go_and_nested_embed_are_inputs(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as directory:
            root = pathlib.Path(directory)
            paths = ['go.mod', 'go.sum', 'internal/config/default.yaml',
                     'configs/config.example.yaml', 'docs/contracts/memory-bounded-fault-matrix.json',
                     'tools/memory-state/runner.sh', 'tools/memory-state/driver.py',
                     'internal/webui/dist/page.html', 'internal/owner/cache/nested/state.txt']
            for name in paths:
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text('original')
            source = root / 'internal/owner/embed.go'
            source.write_text('//go:embed all:cache\npackage owner\n')
            before = fingerprints(root)
            self.assertEqual(set(before), set(paths) | {'internal/owner/embed.go'})
            (root / 'tools/memory-state/runner.sh').write_text('changed actual runner')
            after = fingerprints(root)
            self.assertNotEqual(before, after)
            self.assertNotEqual(before['tools/memory-state/runner.sh'], after['tools/memory-state/runner.sh'])
            for name in paths:
                if not name.endswith('runner.sh'):
                    self.assertEqual(before[name], after[name])


if __name__ == '__main__':
    unittest.main()
