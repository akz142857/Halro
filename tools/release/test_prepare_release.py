#!/usr/bin/env python3

import importlib.util
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "tools/release/prepare_release.py"

CHANGELOG = """# Changelog

Preamble.

## [Unreleased]

### Fixed

- something that was broken

## [1.2.3] - 2026-01-01

### Added

- the previous release

[1.2.3]: https://github.com/akz142857/Halro/compare/v1.2.2...v1.2.3
[1.2.2]: https://github.com/akz142857/Halro/releases/tag/v1.2.2
"""

README = """# Halro

docker pull ghcr.io/akz142857/halro:v1.2.3
docker pull ghcr.io/akz142857/halro-deadman:v1.2.3
gh release download v1.2.3 --repo akz142857/Halro
"""

LICENSE_REVIEW = """# Dependency license review

## Drift gate

- `go.mod`: `1111111111111111111111111111111111111111`
- `web/package.json`: `2222222222222222222222222222222222222222`
- `web/package-lock.json`: `3333333333333333333333333333333333333333`

The two web hashes last moved
for `v1.2.0`, and now `v1.2.3`, each of which bumped the
`version`
field in both files and changed nothing else.
"""


class PrepareReleaseTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.root = Path(self.directory.name)
        self.addCleanup(self.directory.cleanup)
        (self.root / "tools/release").mkdir(parents=True)
        (self.root / "web").mkdir()
        (self.root / "docs/verification/assessments").mkdir(parents=True)
        (self.root / "internal/config").mkdir(parents=True)
        shutil.copy(SCRIPT, self.root / "tools/release/prepare_release.py")
        (self.root / "CHANGELOG.md").write_text(CHANGELOG, encoding="utf-8")
        (self.root / "README.md").write_text(README, encoding="utf-8")
        (self.root / "web/package.json").write_text('{\n  "version": "1.2.3"\n}\n', encoding="utf-8")
        (self.root / "web/package-lock.json").write_text(
            '{\n  "version": "1.2.3",\n  "packages": {\n    "": {\n      "version": "1.2.3"\n    }\n  }\n}\n',
            encoding="utf-8",
        )
        (self.root / "docs/verification/dependency-license-review.md").write_text(LICENSE_REVIEW, encoding="utf-8")
        (self.root / "internal/config/default.yaml").write_text("version: 1\nretry:\n  jitter: true\n", encoding="utf-8")
        for command in (["init", "-q", "-b", "main"], ["add", "-A"], ["-c", "user.email=t@e", "-c", "user.name=t", "commit", "-qm", "fixture"]):
            subprocess.run(["git", "-C", str(self.root), *command], check=True, capture_output=True)

    def run_tool(self, *arguments):
        return subprocess.run(
            ["python3", str(self.root / "tools/release/prepare_release.py"), *arguments],
            capture_output=True, text=True, env={**os.environ, "HOME": str(self.root)},
        )

    def read(self, name):
        return (self.root / name).read_text(encoding="utf-8")

    def test_it_moves_every_surface_a_release_displaces(self):
        result = self.run_tool("v1.2.4", "--date", "2026-02-02")
        self.assertEqual(result.returncode, 0, result.stderr)

        changelog = self.read("CHANGELOG.md")
        self.assertIn("## [1.2.4] - 2026-02-02", changelog)
        self.assertIn("- something that was broken", changelog.split("## [1.2.3]")[0])
        # Unreleased is emptied, not deleted, and keeps its place at the top.
        self.assertRegex(changelog, r"## \[Unreleased\]\n\n## \[1\.2\.4\]")
        self.assertIn("[1.2.4]: https://github.com/akz142857/Halro/compare/v1.2.3...v1.2.4", changelog)

        readme = self.read("README.md")
        self.assertNotIn("v1.2.3", readme)
        self.assertEqual(readme.count("v1.2.4"), 3)

        self.assertIn('"version": "1.2.4"', self.read("web/package.json"))

        # The configuration this release ships is kept, so a later release's
        # retirement table is tested against a real old config rather than an
        # author's idea of one.
        self.assertEqual(
            self.read("internal/config/testdata/releases/v1.2.4.yaml"),
            self.read("internal/config/default.yaml"),
        )
        self.assertEqual(self.read("web/package-lock.json").count('"version": "1.2.4"'), 2)

        review = self.read("docs/verification/dependency-license-review.md")
        # The history sentence must gain the new version without losing the old.
        self.assertIn("`v1.2.3`, and now `v1.2.4`", review)
        # Hashes are recomputed from the files as written, not guessed.
        for name in ("web/package.json", "web/package-lock.json"):
            digest = subprocess.run(
                ["git", "-C", str(self.root), "hash-object", name],
                check=True, capture_output=True, text=True,
            ).stdout.strip()
            self.assertIn(f"- `{name}`: `{digest}`", review)
        # A dependency's own hash is not this script's to move.
        self.assertIn("- `go.mod`: `1111111111111111111111111111111111111111`", review)

        assessment = self.read("docs/verification/assessments/v1.2.4.md")
        self.assertIn("# v1.2.4 pre-release assessment", assessment)
        self.assertIn("TODO", assessment)

    def test_it_refuses_rather_than_guesses(self):
        empty = CHANGELOG.replace("### Fixed\n\n- something that was broken\n\n", "")
        cases = {
            "1.2.4": "version must look like",
            "v1.2.4 with an existing section": None,
        }
        self.assertIn(cases["1.2.4"], self.run_tool("1.2.4").stderr)

        # An empty Unreleased section is a refusal: the release would otherwise
        # publish notes describing nothing.
        (self.root / "CHANGELOG.md").write_text(empty, encoding="utf-8")
        blank = self.run_tool("v1.2.4")
        self.assertNotEqual(blank.returncode, 0)
        self.assertIn("## [Unreleased] is empty", blank.stderr)
        (self.root / "CHANGELOG.md").write_text(CHANGELOG, encoding="utf-8")

        # A version that is already released is never re-prepared.
        already = self.run_tool("v1.2.3")
        self.assertNotEqual(already.returncode, 0)
        self.assertIn("already", already.stderr)

        # A tag that exists means the version was published.
        subprocess.run(["git", "-C", str(self.root), "tag", "v1.2.4"], check=True, capture_output=True)
        tagged = self.run_tool("v1.2.4")
        self.assertNotEqual(tagged.returncode, 0)
        self.assertIn("already exists", tagged.stderr)
        subprocess.run(["git", "-C", str(self.root), "tag", "-d", "v1.2.4"], check=True, capture_output=True)

        # A surface it cannot find is a refusal, not a silent skip. This is the
        # defect the first draft shipped: a replacement that matched nothing and
        # reported success.
        (self.root / "README.md").write_text("# Halro\n\nno versions here\n", encoding="utf-8")
        missing = self.run_tool("v1.2.4")
        self.assertNotEqual(missing.returncode, 0)
        self.assertIn("names no v1.2.3", missing.stderr)

    def test_a_second_run_cannot_double_apply(self):
        self.assertEqual(self.run_tool("v1.2.4", "--date", "2026-02-02").returncode, 0)
        again = self.run_tool("v1.2.4", "--date", "2026-02-02")
        self.assertNotEqual(again.returncode, 0)
        # The second run is caught one step earlier than the section check: the
        # version it was asked to prepare is now the newest released section.
        self.assertIn("is already the newest changelog section", again.stderr)
        # Nothing was half-applied by the refusal.
        self.assertEqual(self.read("CHANGELOG.md").count("## [1.2.4]"), 1)
        self.assertEqual(self.read("README.md").count("v1.2.4"), 3)


    def test_it_refuses_to_resnapshot_a_published_config(self):
        snapshots = self.root / "internal/config/testdata/releases"
        snapshots.mkdir(parents=True)
        (snapshots / "v1.2.4.yaml").write_text("version: 1\n", encoding="utf-8")
        result = self.run_tool("v1.2.4", "--date", "2026-02-02")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("never re-snapshotted", result.stderr)


if __name__ == "__main__":
    unittest.main()
