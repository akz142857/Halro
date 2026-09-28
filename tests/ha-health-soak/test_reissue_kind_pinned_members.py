import base64
import hashlib
import json
import stat
import tempfile
import unittest
from pathlib import Path

import reissue_kind_pinned_members as renewal
import stage_kind_certificates as stage


class ReissuePinnedMembersTests(unittest.TestCase):
    def test_member_certificate_renewal_preserves_live_pins(self):
        with tempfile.TemporaryDirectory() as parent:
            directory = Path(parent)
            source = directory / "source"
            source_digest, _ = stage.stage(source)
            files = stage.SECRET_FILES[renewal.MEMBER_SECRET]
            frozen = {
                "type": "Opaque",
                "metadata": {"namespace": "halro", "name": "halro-ha-cluster-tls",
                             "resourceVersion": "100"},
                "data": {key: base64.b64encode((source / path).read_bytes()).decode("ascii")
                         for key, path in files.items()},
            }
            backup = directory / "member-backup.json"
            raw = (json.dumps(frozen, sort_keys=True) + "\n").encode()
            renewal.write_private(backup, raw)
            backup_digest = hashlib.sha256(raw).hexdigest()
            pins = {name: renewal.spki((source / "cluster" / (name + ".crt")).read_bytes())
                    for name in renewal.MEMBERS}

            wrong = dict(pins)
            wrong["halro-2"] = "sha256:" + "0" * 64
            rejected = directory / "rejected"
            with self.assertRaisesRegex(ValueError, "configured SPKI pin: halro-2"):
                renewal.reissue(rejected, source, source_digest, backup, backup_digest, wrong)
            self.assertFalse(rejected.exists())

            output = directory / "renewed"
            digest = renewal.reissue(output, source, source_digest, backup, backup_digest, pins)
            report = json.loads((output / "stage-report.json").read_text())
            self.assertEqual(digest, hashlib.sha256((output / "stage-report.json").read_bytes()).hexdigest())
            self.assertEqual(report["member_spki_pins"], pins)
            self.assertEqual(report["source_stage_report_sha256"], source_digest)
            self.assertEqual(report["member_backup_sha256"], backup_digest)
            for name in renewal.MEMBERS:
                self.assertEqual((output / "cluster" / (name + ".key")).read_bytes(),
                                 (source / "cluster" / (name + ".key")).read_bytes())
                self.assertEqual(renewal.spki((output / "cluster" / (name + ".crt")).read_bytes()),
                                 pins[name])
                self.assertNotEqual((output / "cluster" / (name + ".crt")).read_bytes(),
                                    (source / "cluster" / (name + ".crt")).read_bytes())
            self.assertEqual((output / "query/ca.crt").read_bytes(),
                             (source / "query/ca.crt").read_bytes())
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o700)
            self.assertEqual(stat.S_IMODE((output / "cluster/halro-2.key").stat().st_mode), 0o600)


if __name__ == "__main__":
    unittest.main()
