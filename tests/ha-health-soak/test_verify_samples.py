import json
import os
import tempfile
import unittest
from pathlib import Path

import verify_samples


START = "2026-09-28T21:00:00Z"
FINISH = "2026-09-28T21:01:00Z"


def evidence(directory):
    directory = Path(directory)
    summary = {
        "kind": "smoke_only", "ha_acceptance": "NOT_RUN",
        "environment": "kind-local", "cluster": "ha",
        "members": ["halro-0", "halro-1"],
        "collection_complete": False, "sampling_continuous": False,
        "health_endpoint_coverage_complete": False,
        "sample_count": 2, "missed_slots": 0,
        "elapsed_seconds": 60.0, "max_monotonic_gap_seconds": 15.0,
        "started_at": START, "finished_at": FINISH,
        "counts": {"observed": 1, "overall_unknown": 1, "unavailable": 1},
    }
    rows = [
        {
            "collected_at": "2026-09-28T21:00:00.1Z", "result": "observed",
            "server_observed_at": "2026-09-28T21:00:00.123456789Z",
            "cards": {name: "unknown" for name in verify_samples.collector.CARDS},
            "members": [
                {"instance": "halro-0", "up": True, "role": "primary",
                 "sampled_at": "2026-09-28T21:00:00.28Z"},
                {"instance": "halro-1", "up": True, "role": "replica",
                 "sampled_at": "2026-09-28T21:00:00Z"},
            ],
            "unexpected_sources": 0,
        },
        {"collected_at": "2026-09-28T21:00:15Z", "result": "unavailable",
         "failure": "ConnectionRefusedError"},
    ]
    write(directory / "summary.json", json.dumps(summary) + "\n")
    write(directory / "samples.jsonl", "".join(json.dumps(row) + "\n" for row in rows))
    return summary, rows


def write(path, content):
    path.write_text(content)
    os.chmod(path, 0o600)


class VerifySamplesTest(unittest.TestCase):
    def test_reconciles_interrupted_run_without_claiming_coverage(self):
        with tempfile.TemporaryDirectory() as directory:
            evidence(directory)
            result = verify_samples.verify(directory)
            self.assertEqual(result["local_structure"], "consistent")
            self.assertEqual(result["counts"]["unavailable"], 1)
            self.assertFalse(result["collection_complete"])
            self.assertFalse(result["health_endpoint_coverage_complete"])
            self.assertEqual(result["ha_acceptance"], "NOT_RUN")

    def test_rejects_complete_coverage_with_failed_sample(self):
        with tempfile.TemporaryDirectory() as directory:
            summary, _ = evidence(directory)
            summary.update(collection_complete=True, sampling_continuous=True,
                           health_endpoint_coverage_complete=True)
            write(Path(directory) / "summary.json", json.dumps(summary) + "\n")
            with self.assertRaisesRegex(verify_samples.EvidenceError, "unavailable"):
                verify_samples.verify(directory)

    def test_rejects_candidate_with_short_wall_window(self):
        with tempfile.TemporaryDirectory() as directory:
            summary, _ = evidence(directory)
            summary.update(kind="candidate_observation_only", collection_complete=True,
                           elapsed_seconds=259200, candidate_sha="b" * 40,
                           image_digest="sha256:" + "a" * 64,
                           config_sha256="a" * 64, rules_sha256="a" * 64)
            write(Path(directory) / "summary.json", json.dumps(summary) + "\n")
            with self.assertRaisesRegex(verify_samples.EvidenceError, "shorter than 72 hours"):
                verify_samples.verify(directory)

    def test_rejects_malformed_candidate_digest_without_crashing(self):
        with tempfile.TemporaryDirectory() as directory:
            summary, _ = evidence(directory)
            summary.update(kind="candidate_observation_only", candidate_sha="b" * 40,
                           image_digest=None, config_sha256="a" * 64,
                           rules_sha256="a" * 64)
            write(Path(directory) / "summary.json", json.dumps(summary) + "\n")
            with self.assertRaisesRegex(verify_samples.EvidenceError, "image digest"):
                verify_samples.verify(directory)

    def test_rejects_wall_gap_when_continuity_is_claimed(self):
        with tempfile.TemporaryDirectory() as directory:
            summary, rows = evidence(directory)
            summary.update(collection_complete=True, sampling_continuous=True)
            rows[1]["collected_at"] = "2026-09-28T21:00:45Z"
            write(Path(directory) / "summary.json", json.dumps(summary) + "\n")
            write(Path(directory) / "samples.jsonl", "".join(json.dumps(row) + "\n" for row in rows))
            with self.assertRaisesRegex(verify_samples.EvidenceError, "continuous coverage"):
                verify_samples.verify(directory)

    def test_rejects_changed_counts_duplicate_keys_and_truncated_line(self):
        with tempfile.TemporaryDirectory() as directory:
            summary, rows = evidence(directory)
            summary["counts"]["unavailable"] = 0
            write(Path(directory) / "summary.json", json.dumps(summary) + "\n")
            with self.assertRaisesRegex(verify_samples.EvidenceError, "counts differ"):
                verify_samples.verify(directory)

            summary["counts"]["unavailable"] = 1
            write(Path(directory) / "summary.json", json.dumps(summary) + "\n")
            duplicate = json.dumps(rows[0]).replace('"result": "observed"',
                                                  '"result": "observed", "result": "observed"')
            write(Path(directory) / "samples.jsonl", duplicate + "\n" + json.dumps(rows[1]) + "\n")
            with self.assertRaisesRegex(verify_samples.EvidenceError, "duplicate JSON key"):
                verify_samples.verify(directory)

            write(Path(directory) / "samples.jsonl", json.dumps(rows[0]) + "\n" + json.dumps(rows[1]))
            with self.assertRaisesRegex(verify_samples.EvidenceError, "incomplete"):
                verify_samples.verify(directory)


if __name__ == "__main__":
    unittest.main()
