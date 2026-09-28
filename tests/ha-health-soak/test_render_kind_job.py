import contextlib
import io
import json
import os
import tempfile
import unittest
from pathlib import Path

import render_kind_job


SHA = "a" * 64


def arguments(path, **overrides):
    values = {
        "run-name": "ha-health-soak-test",
        "namespace": "halro-monitoring",
        "url": "https://ha-health.halro-monitoring.svc.cluster.local:9105/",
        "secret-name": "ha-health-tls",
        "environment": "kind-local",
        "cluster": "halro-kind-health-local",
        "members": "halro-0,halro-1,halro-2",
        "candidate-sha": "b" * 40,
        "image-digest": "sha256:" + SHA,
        "config-sha256": SHA,
        "rules-sha256": SHA,
        "python-image": "python:3.12-slim@sha256:" + SHA,
        "manifest": str(path),
    }
    values.update(overrides)
    return [item for key, value in values.items() for item in ("--" + key, value)]


class RenderKindJobTest(unittest.TestCase):
    def test_manifest_has_isolated_job_and_no_secret_bytes(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "job.json"
            args = render_kind_job.parse_args(arguments(path))
            manifest = render_kind_job.render(args, render_kind_job.SCRIPT.read_text())
            config, pvc, job = manifest["items"]
            self.assertTrue(config["immutable"])
            self.assertEqual(pvc["spec"]["accessModes"], ["ReadWriteOnce"])
            self.assertEqual(job["spec"]["backoffLimit"], 0)
            pod = job["spec"]["template"]["spec"]
            self.assertFalse(pod["automountServiceAccountToken"])
            self.assertEqual(pod["restartPolicy"], "Never")
            sampler = pod["containers"][0]
            self.assertTrue(sampler["securityContext"]["runAsNonRoot"])
            self.assertTrue(sampler["securityContext"]["readOnlyRootFilesystem"])
            self.assertEqual(sampler["securityContext"]["capabilities"]["drop"], ["ALL"])
            self.assertEqual(pod["volumes"][2]["emptyDir"]["medium"], "Memory")
            self.assertEqual(pod["volumes"][1]["secret"]["items"], [
                {"key": key, "path": key} for key in
                ("ca.crt", "collector.crt", "collector.key")])
            self.assertNotIn("secretKeyRef", json.dumps(manifest))
            self.assertNotIn("private-key", json.dumps(manifest))
            self.assertIn("--candidate-sha", sampler["args"])

    def test_rejects_external_url_and_unpinned_image(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "job.json"
            for override in ({"url": "https://example.com/"},
                             {"python-image": "python:3.12-slim"},
                             {"run-name": "INVALID"},
                             {"candidate-sha": "short"}):
                with self.subTest(override=override), contextlib.redirect_stderr(io.StringIO()):
                    with self.assertRaises(SystemExit) as error:
                        render_kind_job.parse_args(arguments(path, **override))
                    self.assertEqual(error.exception.code, 2)

    def test_writes_private_manifest_once(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "job.json"
            with contextlib.redirect_stdout(io.StringIO()):
                render_kind_job.main(arguments(path))
            self.assertEqual(os.stat(path).st_mode & 0o777, 0o600)
            self.assertEqual(len(json.loads(path.read_text())["items"]), 3)
            with contextlib.redirect_stdout(io.StringIO()):
                with self.assertRaises(FileExistsError):
                    render_kind_job.main(arguments(path))


if __name__ == "__main__":
    unittest.main()
