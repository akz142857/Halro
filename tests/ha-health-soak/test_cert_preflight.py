import base64
import importlib.util
import json
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch


MODULE_PATH = Path(__file__).with_name("cert_preflight.py")
SPEC = importlib.util.spec_from_file_location("ha_health_cert_preflight", MODULE_PATH)
preflight = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(preflight)


class CertificatePreflightTests(unittest.TestCase):
    def pem(self, body):
        return b"-----BEGIN CERTIFICATE-----\n" + body + b"\n-----END CERTIFICATE-----\n"

    def manifest(self):
        return {"version": 1, "context": "kind-local", "secrets": [
            {"namespace": "halro", "name": "member-tls", "cert_keys": ["ca.crt", "node.crt"]},
        ]}

    def test_exact_inventory_and_expiry_are_required(self):
        manifest = self.manifest()
        data = {"ca.crt": base64.b64encode(self.pem(b"Y2E=")).decode(),
                "node.crt": base64.b64encode(self.pem(b"bm9kZQ==")).decode()}
        fetch = lambda context, namespace, name: data
        inspect = lambda pem: (500, 2000 if b"Y2E=" in pem else 2100, "a" * 64)
        passed = preflight.check_window(manifest, "b" * 64, 900, fetcher=fetch, inspector=inspect, now=1000)
        self.assertEqual(passed["status"], "ready_for_window")
        self.assertEqual(len(passed["certificates"]), 2)
        self.assertEqual(passed["failures"], [])

        expired = preflight.check_window(manifest, "b" * 64, 1100, fetcher=fetch, inspector=inspect, now=1000)
        self.assertEqual(expired["status"], "certificate_window_blocked")
        self.assertIn("halro/member-tls/ca.crt:chain_0:expires_before_required_window", expired["failures"])
        self.assertTrue(all(row["secret"] == "halro/member-tls" for row in expired["certificates"]))

        for incomplete in ({"ca.crt": data["ca.crt"]}, {**data, "unexpected.crt": data["ca.crt"]}):
            report = preflight.check_window(manifest, "b" * 64, 900,
                                            fetcher=lambda *_: incomplete, inspector=inspect, now=1000)
            self.assertEqual(report["status"], "certificate_window_blocked")
            self.assertEqual(report["certificates"], [])
            self.assertEqual(len(report["failures"]), 1)

    def test_invalid_manifest_and_certificate_fail_closed(self):
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / "manifest.json"
            manifest = self.manifest()
            manifest["secrets"].append(dict(manifest["secrets"][0]))
            path.write_text(json.dumps(manifest))
            with self.assertRaisesRegex(ValueError, "duplicate"):
                preflight.load_manifest(path)
            manifest["secrets"].pop()
            path.write_text(json.dumps(manifest))
            loaded, digest = preflight.load_manifest(path)
            self.assertEqual(loaded, manifest)
            self.assertEqual(len(digest), 64)
            bad_data = {"ca.crt": "not-base64", "node.crt": base64.b64encode(self.pem(b"bm9kZQ==")).decode()}
            report = preflight.check_window(manifest, digest, 900,
                                            fetcher=lambda *_: bad_data, inspector=lambda _: (500, 2000, "a" * 64), now=1000)
            self.assertEqual(report["status"], "certificate_window_blocked")
            self.assertEqual(len(report["failures"]), 1)

    def test_every_certificate_in_a_chain_must_cover_the_window(self):
        manifest = self.manifest()
        data = {"ca.crt": base64.b64encode(self.pem(b"Y2E=") + self.pem(b"b2xk")).decode(),
                "node.crt": base64.b64encode(self.pem(b"bm9kZQ==")).decode()}
        inspect = lambda pem: (500, 1500 if b"b2xk" in pem else 3000, "a" * 64)
        report = preflight.check_window(manifest, "b" * 64, 900,
                                        fetcher=lambda *_: data, inspector=inspect, now=1000)
        self.assertEqual(report["status"], "certificate_window_blocked")
        self.assertEqual(len(report["certificates"]), 3)
        self.assertIn("halro/member-tls/ca.crt:chain_1:expires_before_required_window", report["failures"])
        future = preflight.check_window(manifest, "b" * 64, 900, fetcher=lambda *_: data,
                                        inspector=lambda _: (1100, 3000, "a" * 64), now=1000)
        self.assertEqual(future["status"], "certificate_window_blocked")
        self.assertIn("halro/member-tls/ca.crt:chain_0:not_yet_valid", future["failures"])

    def test_kubectl_only_returns_public_certificate_values(self):
        calls = []

        def fake_run(command, **_kwargs):
            calls.append(command)
            output = (b"halro/member-tls|ca.crt,ca.key,node.crt,node.key,"
                      if len(calls) == 1 else b"Y2VydA==")
            return subprocess.CompletedProcess(command, 0, stdout=output)

        with patch.object(preflight.subprocess, "run", side_effect=fake_run):
            certs = preflight.fetch_secret("kind-local", "halro", "member-tls")
        self.assertEqual(set(certs), {"ca.crt", "node.crt"})
        self.assertEqual(len(calls), 3)
        self.assertTrue(all(".key" not in " ".join(command) for command in calls[1:]))

    def test_public_certificate_file_is_part_of_the_same_window(self):
        files = preflight.parse_certificate_files(["operator=/private/operator.crt"])
        self.assertEqual(files, [("operator", "/private/operator.crt")])
        with self.assertRaisesRegex(ValueError, "unique label"):
            preflight.parse_certificate_files(["operator=/private/a.crt", "operator=/private/b.crt"])
        with self.assertRaisesRegex(ValueError, "clean absolute"):
            preflight.parse_certificate_files(["operator=relative.crt"])
        data = {"ca.crt": base64.b64encode(self.pem(b"Y2E=")).decode(),
                "node.crt": base64.b64encode(self.pem(b"bm9kZQ==")).decode()}
        report = preflight.check_window(self.manifest(), "b" * 64, 900, fetcher=lambda *_: data,
                                        inspector=lambda pem: (500, 1500 if b"b3A=" in pem else 3000, "a" * 64),
                                        now=1000, files=files, reader=lambda _: self.pem(b"b3A="))
        self.assertEqual(report["status"], "certificate_window_blocked")
        self.assertEqual(report["file_count"], 1)
        self.assertIn("file:operator:chain_0:expires_before_required_window", report["failures"])

    def test_openssl_certificate_parser_uses_real_not_after(self):
        with tempfile.TemporaryDirectory() as root:
            cert, key = Path(root) / "cert.pem", Path(root) / "key.pem"
            subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
                            "-subj", "/CN=ha-preflight-test", "-keyout", str(key), "-out", str(cert)],
                           check=True, capture_output=True)
            starts, expires, fingerprint = preflight.inspect_certificate(cert.read_bytes())
            self.assertLess(starts, expires)
            self.assertGreater(expires, 0)
            self.assertEqual(len(fingerprint), 64)
            self.assertEqual(len(preflight.split_certificate_chain(cert.read_bytes() + cert.read_bytes())), 2)


if __name__ == "__main__":
    unittest.main()
