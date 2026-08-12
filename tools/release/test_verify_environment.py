#!/usr/bin/env python3

import unittest

from tools.release.verify_environment import EnvironmentVerificationError, verify_environment


def valid_environment():
    return {
        "name": "v1-release",
        "protection_rules": [
            {
                "type": "required_reviewers",
                "prevent_self_review": True,
                "reviewers": [{"type": "Team", "reviewer": {"slug": "release-reviewers"}}],
            }
        ],
    }


class VerifyEnvironmentTests(unittest.TestCase):
    def test_protected_environment_passes(self):
        self.assertEqual(verify_environment(valid_environment(), "v1-release"), 1)

    def test_missing_required_reviewers_fails(self):
        payload = valid_environment()
        payload["protection_rules"] = []
        with self.assertRaises(EnvironmentVerificationError):
            verify_environment(payload, "v1-release")

    def test_empty_required_reviewers_fails(self):
        payload = valid_environment()
        payload["protection_rules"][0]["reviewers"] = []
        with self.assertRaises(EnvironmentVerificationError):
            verify_environment(payload, "v1-release")

    def test_self_review_fails(self):
        payload = valid_environment()
        payload["protection_rules"][0]["prevent_self_review"] = False
        with self.assertRaises(EnvironmentVerificationError):
            verify_environment(payload, "v1-release")

    def test_wrong_environment_fails(self):
        with self.assertRaises(EnvironmentVerificationError):
            verify_environment(valid_environment(), "production")


if __name__ == "__main__":
    unittest.main()
