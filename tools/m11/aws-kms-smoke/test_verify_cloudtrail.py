import importlib.util
import json
import pathlib
import tempfile
import unittest


SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location(
    "verify_cloudtrail", SCRIPT_DIR / "verify_cloudtrail.py"
)
VERIFY = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VERIFY)


class VerifyCloudTrailTest(unittest.TestCase):
    def write_json(self, directory, name, value):
        path = pathlib.Path(directory) / name
        path.write_text(json.dumps(value), encoding="utf-8")
        return path

    def test_correlates_only_exact_kms_request_ids(self):
        with tempfile.TemporaryDirectory() as directory:
            private = self.write_json(
                directory,
                "private.json",
                {
                    "encrypt_request_id": "encrypt-request",
                    "decrypt_request_id": "decrypt-request",
                },
            )
            encrypt = self.write_json(
                directory,
                "encrypt.json",
                {
                    "Events": [
                        {
                            "CloudTrailEvent": json.dumps(
                                {
                                    "eventSource": "kms.amazonaws.com",
                                    "eventName": "Encrypt",
                                    "requestID": "encrypt-request",
                                }
                            )
                        }
                    ]
                },
            )
            decrypt = self.write_json(
                directory,
                "decrypt.json",
                {
                    "Events": [
                        {
                            "CloudTrailEvent": json.dumps(
                                {
                                    "eventSource": "kms.amazonaws.com",
                                    "eventName": "Decrypt",
                                    "requestID": "wrong-request",
                                }
                            )
                        },
                        {"CloudTrailEvent": "not-json"},
                    ]
                },
            )
            self.assertEqual(
                VERIFY.correlate(private, encrypt, decrypt),
                {
                    "cloudtrail_encrypt_correlated": True,
                    "cloudtrail_decrypt_correlated": False,
                },
            )


if __name__ == "__main__":
    unittest.main()
