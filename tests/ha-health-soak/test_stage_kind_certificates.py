import importlib.util
import json
import stat
import tempfile
import unittest
from pathlib import Path


SPEC = importlib.util.spec_from_file_location(
    "ha_health_stage_kind_certificates", Path(__file__).with_name("stage_kind_certificates.py")
)
stager = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(stager)


class StageKindCertificatesTests(unittest.TestCase):
    def test_stage_is_private_complete_and_never_installs(self):
        with tempfile.TemporaryDirectory() as parent:
            output = Path(parent) / "new-certificates"
            digest, count = stager.stage(output)
            report = json.loads((output / "stage-report.json").read_text())
            self.assertEqual(count, 9)
            self.assertEqual(len(digest), 64)
            self.assertEqual(report["scope"], "staged local kind certificates only; not installed or accepted")
            self.assertEqual(set(report["secret_files"]), {
                "halro/halro-ha-cluster-tls", "halro-monitoring/ha-health-tls",
                "halro-monitoring/ha-prometheus-tls", "halro-monitoring/ha-prometheus-web-tls",
                "halro-monitoring/ha-health-prometheus-client",
            })
            for files in (*report["secret_files"].values(), report["operator_files"]):
                for path in files.values():
                    self.assertTrue((output / path).is_file(), path)
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o700)
            for path in output.rglob("*.key"):
                self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            self.assertNotIn("PRIVATE KEY", (output / "stage-report.json").read_text())

    def test_existing_output_is_rejected(self):
        with tempfile.TemporaryDirectory() as parent:
            with self.assertRaisesRegex(ValueError, "new absolute directory"):
                stager.stage(Path(parent))


if __name__ == "__main__":
    unittest.main()
