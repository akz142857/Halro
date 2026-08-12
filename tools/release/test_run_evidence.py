#!/usr/bin/env python3

import json
import tempfile
import unittest
from pathlib import Path

from tools.release.run_evidence import RunEvidenceError, create_manifest, verify_manifest


class RunEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.temporary_directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary_directory.cleanup)
        self.release_dir = Path(self.temporary_directory.name)
        (self.release_dir / "halro-linux-amd64.tar.gz").write_bytes(b"binary archive")
        (self.release_dir / "checksums.txt").write_text("checksums\n", encoding="utf-8")

    def manifest(self):
        return create_manifest(
            self.release_dir,
            run_id="12345",
            run_attempt="1",
            commit="a" * 40,
            ref="refs/tags/v1.0.0-rc.2",
            workflow_ref="akz142857/Halro/.github/workflows/release.yml@refs/tags/v1.0.0-rc.2",
        )

    def test_round_trip_passes(self):
        manifest = json.loads(json.dumps(self.manifest()))
        verify_manifest(manifest, self.release_dir)

    def test_tampered_artifact_fails(self):
        manifest = self.manifest()
        (self.release_dir / "checksums.txt").write_text("tampered\n", encoding="utf-8")
        with self.assertRaises(RunEvidenceError):
            verify_manifest(manifest, self.release_dir)

    def test_missing_artifact_fails(self):
        manifest = self.manifest()
        (self.release_dir / "checksums.txt").unlink()
        with self.assertRaises(RunEvidenceError):
            verify_manifest(manifest, self.release_dir)

    def test_unrecorded_artifact_fails(self):
        manifest = self.manifest()
        (self.release_dir / "unexpected.txt").write_text("unexpected", encoding="utf-8")
        with self.assertRaises(RunEvidenceError):
            verify_manifest(manifest, self.release_dir)

    def test_invalid_run_identity_fails(self):
        with self.assertRaises(RunEvidenceError):
            create_manifest(
                self.release_dir,
                run_id="0",
                run_attempt="1",
                commit="short",
                ref="main",
                workflow_ref="release.yml",
            )


if __name__ == "__main__":
    unittest.main()
