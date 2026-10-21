import importlib.util
import io
import json
import os
import tempfile
import unittest
from contextlib import redirect_stderr
from pathlib import Path
from unittest.mock import patch


MODULE_PATH = Path(__file__).with_name("collector.py")
SPEC = importlib.util.spec_from_file_location("ha_health_soak_collector", MODULE_PATH)
collector = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(collector)


class CollectorTests(unittest.TestCase):
    def options(self, output):
        return collector.parse_options([
            "--url", "https://ha-health.example.internal/", "--ca", "ca.pem",
            "--cert", "operator.crt", "--key", "operator.key",
            "--environment", "kind-local", "--cluster", "ha",
            "--members", "halro-0,halro-1", "--duration-seconds", "0.16",
            "--interval-seconds", "0.05", "--output", str(output),
        ])

    def payload(self):
        return {
            "environment": "kind-local", "cluster": "ha", "expected_members": 2,
            "observed_at": collector.utc_now(),
            "members": [
                {"instance": "halro-0", "up": True, "role": "primary", "sampled_at": collector.utc_now()},
                {"instance": "halro-1", "up": False, "role": "replica", "sampled_at": collector.utc_now()},
            ],
            **{name: {"level": "unknown"} for name in collector.CARDS},
        }

    def test_rejects_unqualified_72_hour_run_and_identity_mismatch(self):
        with tempfile.TemporaryDirectory() as root:
            options = self.options(Path(root) / "samples")
            with self.assertRaisesRegex(ValueError, "cluster_identity_mismatch"):
                collector.summarize_health({**self.payload(), "cluster": "other"}, options)
            with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                collector.parse_options([
                    "--url", "https://ha-health.example.internal/", "--ca", "ca.pem",
                    "--cert", "operator.crt", "--key", "operator.key",
                    "--environment", "kind-local", "--cluster", "ha", "--members", "halro-0,halro-1",
                    "--duration-seconds", str(collector.FORMAL_DURATION), "--output", str(Path(root) / "formal"),
                ])

    def test_accepts_go_nanosecond_observation_time_and_rejects_invalid_precision(self):
        with tempfile.TemporaryDirectory() as root:
            options = self.options(Path(root) / "samples")
            payload = self.payload()
            payload["observed_at"] = (
                collector.dt.datetime.now(collector.dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f") + "123Z"
            )
            self.assertEqual(collector.summarize_health(payload, options)["server_observed_at"], payload["observed_at"])
            payload["observed_at"] = collector.dt.datetime.now(collector.dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S") + ".28Z"
            self.assertEqual(collector.summarize_health(payload, options)["server_observed_at"], payload["observed_at"])
            payload["observed_at"] = "2026-09-28T21:00:00.1234567890Z"
            with self.assertRaisesRegex(ValueError, "observation_time_invalid"):
                collector.summarize_health(payload, options)
            payload["observed_at"] = collector.utc_now().removesuffix("Z")
            with self.assertRaisesRegex(ValueError, "observation_time_invalid"):
                collector.summarize_health(payload, options)

    def test_persists_bounded_observations_without_claiming_ha_acceptance(self):
        with tempfile.TemporaryDirectory() as root:
            output = Path(root) / "samples"
            options = self.options(output)
            now = [0.0]

            class FakeContext:
                def load_cert_chain(self, _cert, _key):
                    pass

            def fetcher(_options, _context):
                return self.payload()

            with patch.object(collector.ssl, "create_default_context", return_value=FakeContext()):
                summary = collector.collect(options, fetcher=fetcher, clock=lambda: now[0], sleeper=lambda seconds: now.__setitem__(0, now[0] + seconds))
            rows = [json.loads(line) for line in (output / "samples.jsonl").read_text().splitlines()]
            self.assertEqual(len(rows), 4)
            self.assertEqual(summary["counts"]["overall_unknown"], 4)
            self.assertEqual(summary["ha_acceptance"], "NOT_RUN")
            self.assertEqual(summary["kind"], "smoke_only")
            self.assertTrue(summary["collection_complete"])
            self.assertTrue(summary["sampling_continuous"])
            self.assertTrue(summary["health_endpoint_coverage_complete"])
            self.assertEqual(json.loads((output / "summary.json").read_text()), summary)
            self.assertEqual(os.stat(output).st_mode & 0o777, 0o700)
            self.assertEqual(os.stat(output / "samples.jsonl").st_mode & 0o777, 0o600)
            self.assertNotIn("operator.key", (output / "samples.jsonl").read_text())

    def test_source_failure_and_scheduling_gap_cannot_claim_complete_coverage(self):
        class FakeContext:
            def load_cert_chain(self, _cert, _key):
                pass

        with tempfile.TemporaryDirectory() as root, patch.object(collector.ssl, "create_default_context", return_value=FakeContext()):
            now = [0.0]
            options = self.options(Path(root) / "source-down")

            def unavailable(_options, _context):
                raise ValueError("http_503")

            result = collector.collect(options, fetcher=unavailable, clock=lambda: now[0], sleeper=lambda seconds: now.__setitem__(0, now[0] + seconds))
            self.assertTrue(result["sampling_continuous"])
            self.assertFalse(result["health_endpoint_coverage_complete"])
            self.assertEqual(result["counts"]["unavailable"], 4)
            self.assertEqual(json.loads((options.output / "samples.jsonl").read_text().splitlines()[0])["failure"], "http_503")

            now[0] = 0.0
            options = self.options(Path(root) / "gap")

            def too_slow(_options, _context):
                now[0] += 0.2
                return self.payload()

            result = collector.collect(options, fetcher=too_slow, clock=lambda: now[0], sleeper=lambda seconds: now.__setitem__(0, now[0] + seconds))
            self.assertTrue(result["collection_complete"])
            self.assertFalse(result["sampling_continuous"])
            self.assertFalse(result["health_endpoint_coverage_complete"])
            self.assertGreater(result["missed_slots"], 0)


if __name__ == "__main__":
    unittest.main()
