import os
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts/archive-release-run.sh"


class ArchiveReleaseRunTest(unittest.TestCase):
    def run_with_artifacts(self, artifact_json: str) -> tuple[subprocess.CompletedProcess[str], Path, str]:
        fixture = tempfile.TemporaryDirectory()
        self.addCleanup(fixture.cleanup)
        base = Path(fixture.name)
        log = base / "gh.log"
        fake_gh = base / "gh"
        fake_gh.write_text(
            "#!/bin/sh\n"
            "printf '%s\\n' \"$*\" >>\"$GH_CALL_LOG\"\n"
            "if [ \"$1 $2 $4\" = 'run view --json' ]; then\n"
            "  printf '%s\\n' '{\"attempt\":2,\"headSha\":\"abc\"}'\n"
            "elif [ \"$1\" = api ]; then\n"
            "  printf '%s\\n' \"$ARTIFACT_JSON\"\n"
            "elif [ \"$1 $2 $4\" = 'run view --log' ]; then\n"
            "  printf '%s\\n' log\n"
            "elif [ \"$1 $2\" = 'run download' ]; then\n"
            "  [ \"$5\" = release-assets ] && exit 0\n"
            "  exit 42\n"
            "fi\n"
        )
        fake_gh.chmod(0o755)
        output = base / "archive"
        env = os.environ.copy()
        env.update(
            PATH=f"{base}:{env['PATH']}",
            GH_CALL_LOG=str(log),
            ARTIFACT_JSON=artifact_json,
            TMPDIR=str(base),
        )
        result = subprocess.run(
            [str(SCRIPT), "123", str(output)],
            cwd=ROOT,
            env=env,
            text=True,
            capture_output=True,
        )
        calls = log.read_text() if log.exists() else ""
        return result, output, calls

    def test_dry_run_evidence_name_is_selected_without_leaving_partial_output(self) -> None:
        result, output, calls = self.run_with_artifacts(
            '{"artifacts":['
            '{"name":"release-assets","expired":false},'
            '{"name":"release-run-evidence-123-2-dry-run","expired":false}'
            ']}'
        )
        self.assertEqual(result.returncode, 42)
        self.assertIn("--name release-run-evidence-123-2-dry-run", calls)
        self.assertFalse(output.exists())

    def test_missing_evidence_fails_before_download_or_output_creation(self) -> None:
        result, output, calls = self.run_with_artifacts(
            '{"artifacts":[{"name":"release-assets","expired":false}]}'
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("run download", calls)
        self.assertFalse(output.exists())


if __name__ == "__main__":
    unittest.main()
