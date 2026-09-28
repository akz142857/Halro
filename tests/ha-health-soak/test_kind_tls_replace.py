import base64
import importlib.util
import json
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location(
    "ha_health_kind_tls_replace", Path(__file__).with_name("kind_tls_replace.py")
)
rotation = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(rotation)


def encoded(values):
    return {key: base64.b64encode(value).decode() for key, value in values.items()}


class KindTLSReplaceTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        root = Path(self.temporary.name)
        self.stage = root / "stage"
        self.overlap = root / "overlap"
        self.stage.mkdir(mode=0o700)
        self.overlap.mkdir(mode=0o700)
        (self.stage / "cluster").mkdir(mode=0o700)
        (self.stage / "cluster" / "ca.crt").write_bytes(b"new-ca")
        (self.stage / "cluster" / "server.crt").write_bytes(b"new-cert")
        (self.stage / "cluster" / "server.key").write_bytes(b"new-key")
        (self.overlap / "cluster-old-ca.crt").write_bytes(b"old-ca")
        (self.overlap / "cluster-overlap-ca.crt").write_bytes(b"old-ca\nnew-ca")
        (self.overlap / "halro-monitoring-health-server.crt").write_bytes(b"old-cert")
        self.identity = "halro-monitoring/health"
        self.report = {"secret_files": {self.identity: {
            "ca.crt": "cluster/ca.crt", "server.crt": "cluster/server.crt",
            "server.key": "cluster/server.key"}}}
        self.secret = {"metadata": {"namespace": "halro-monitoring", "name": "health",
                                    "resourceVersion": "7"}, "type": "Opaque",
                       "data": encoded({"ca.crt": b"old-ca", "server.crt": b"old-cert",
                                        "server.key": b"old-key"})}

    def plan(self, secret, phase):
        return rotation.plan_secret(secret, self.identity, phase,
                                    self.stage, self.overlap, self.report)

    def secret_with_data(self, values):
        return {**self.secret, "data": encoded(values)}

    def test_three_phases_change_only_expected_keys(self):
        trust, trust_data, changed = self.plan(self.secret, "trust")
        self.assertEqual(changed, ["ca.crt"])
        self.assertEqual(trust["metadata"]["resourceVersion"], "7")
        self.assertEqual(trust_data, {"ca.crt": b"old-ca\nnew-ca", "server.crt": b"old-cert",
                                      "server.key": b"old-key"})
        leaves, leaves_data, changed = self.plan(self.secret_with_data(trust_data), "leaves")
        self.assertEqual(changed, ["server.crt", "server.key"])
        self.assertEqual(leaves_data["ca.crt"], trust_data["ca.crt"])
        self.assertEqual(leaves_data["server.key"], b"new-key")
        final, final_data, changed = self.plan(self.secret_with_data(leaves_data), "final")
        self.assertEqual(changed, ["ca.crt"])
        self.assertEqual(final_data, {"ca.crt": b"new-ca", "server.crt": b"new-cert",
                                      "server.key": b"new-key"})
        _again, same, changed = self.plan(self.secret_with_data(final_data), "final")
        self.assertEqual(same, final_data)
        self.assertEqual(changed, [])

    def test_unknown_or_partial_state_fails_closed(self):
        with self.assertRaisesRegex(ValueError, "unknown certificate"):
            self.plan(self.secret_with_data({"ca.crt": b"old-ca\nnew-ca",
                                             "server.crt": b"unexpected", "server.key": b"old-key"}), "leaves")
        with self.assertRaisesRegex(ValueError, "partially changed"):
            self.plan(self.secret_with_data({"ca.crt": b"old-ca\nnew-ca",
                                             "server.crt": b"new-cert", "server.key": b"old-key"}), "leaves")
        with self.assertRaisesRegex(ValueError, "requires every new leaf"):
            self.plan(self.secret_with_data({"ca.crt": b"old-ca\nnew-ca",
                                             "server.crt": b"old-cert", "server.key": b"old-key"}), "final")
        annotated = {**self.secret, "metadata": {**self.secret["metadata"], "annotations": {
            "kubectl.kubernetes.io/last-applied-configuration": "data with private key"}}}
        with self.assertRaisesRegex(ValueError, "last-applied"):
            self.plan(annotated, "trust")

    def test_pinned_member_renewal_changes_all_certs_without_changing_keys(self):
        identity = "halro/halro-ha-cluster-tls"
        files = {"ca.crt": "cluster/ca.crt"}
        live = {"ca.crt": b"old-ca\nnew-ca"}
        for number in range(3):
            name = "halro-" + str(number)
            cert_key, private_key = name + ".crt", name + ".key"
            files[cert_key] = "cluster/" + cert_key
            files[private_key] = "cluster/" + private_key
            live[cert_key] = ("old-" + name).encode()
            live[private_key] = ("same-key-" + name).encode()
            (self.stage / files[cert_key]).write_bytes(("new-" + name).encode())
            (self.stage / files[private_key]).write_bytes(live[private_key])
            (self.overlap / ("halro-halro-ha-cluster-tls-" + cert_key)).write_bytes(live[cert_key])
        report = {"secret_files": {identity: files},
                  "member_key_mode": "preserved_configured_spki_for_routine_renewal",
                  "member_spki_pins": {"halro-" + str(i): "sha256:" + str(i) * 64
                                       for i in range(3)}}
        def pinned_spki(material, private=False):
            number = material.decode().rsplit("halro-", 1)[1]
            return report["member_spki_pins"]["halro-" + number]

        patcher = patch.object(rotation, "spki_sha256", side_effect=pinned_spki)
        patcher.start()
        self.addCleanup(patcher.stop)
        secret = {"metadata": {"namespace": "halro", "name": "halro-ha-cluster-tls",
                               "resourceVersion": "10"}, "type": "Opaque", "data": encoded(live)}
        with self.assertRaisesRegex(ValueError, "pinned SPKI renewal stage"):
            rotation.plan_secret(secret, identity, "leaves", self.stage, self.overlap,
                                 {"secret_files": {identity: files}})
        _replacement, expected, changed = rotation.plan_secret(
            secret, identity, "leaves", self.stage, self.overlap, report)
        self.assertEqual(changed, ["halro-0.crt", "halro-1.crt", "halro-2.crt"])
        self.assertEqual(expected["halro-2.key"], live["halro-2.key"])

        mixed = dict(live)
        mixed["halro-0.crt"] = expected["halro-0.crt"]
        with self.assertRaisesRegex(ValueError, "partially changed"):
            rotation.plan_secret({**secret, "data": encoded(mixed)}, identity, "leaves",
                                 self.stage, self.overlap, report)
        wrong_key = dict(live)
        wrong_key["halro-1.key"] = b"unexpected"
        with self.assertRaisesRegex(ValueError, "private key differs"):
            rotation.plan_secret({**secret, "data": encoded(wrong_key)}, identity, "leaves",
                                 self.stage, self.overlap, report)
        (self.stage / "cluster/halro-2.crt").write_bytes(b"new-halro-0")
        with self.assertRaisesRegex(ValueError, "configured SPKI pin: halro-2"):
            rotation.plan_secret(secret, identity, "leaves", self.stage, self.overlap, report)

    def test_backup_is_private_and_exclusive(self):
        backup = Path(self.temporary.name) / "backup"
        backup.mkdir(mode=0o700)
        digest = rotation.write_backup(backup, self.identity, "trust", self.secret)
        self.assertEqual(len(digest), 64)
        path, = backup.iterdir()
        self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
        self.assertEqual(base64.b64decode(json.loads(path.read_bytes())["data"]["server.key"]), b"old-key")
        with self.assertRaises(FileExistsError):
            rotation.write_backup(backup, self.identity, "trust", self.secret)

    def test_kubectl_receives_secret_only_on_stdin(self):
        replacement, _expected, _changed = self.plan(self.secret, "trust")
        commands = []

        def fake_run(command, **options):
            commands.append((command, options))
            return subprocess.CompletedProcess(command, 0, stdout=b"secret/health\n")

        with patch.object(rotation.subprocess, "run", side_effect=fake_run):
            rotation.replace_secret("halro-monitoring", replacement, dry_run=True)
        command, options = commands[0]
        self.assertIn("--dry-run=server", command)
        self.assertEqual(command[-4:], ["-f", "-", "-o", "name"])
        self.assertNotIn("new-ca", " ".join(command))
        self.assertIn(b"b2xkLWtleQ==", options["input"])


if __name__ == "__main__":
    unittest.main()
