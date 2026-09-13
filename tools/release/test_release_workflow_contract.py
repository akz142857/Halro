#!/usr/bin/env python3

from pathlib import Path
import unittest


WORKFLOW = Path(__file__).resolve().parents[2] / ".github" / "workflows" / "release.yml"
CI_WORKFLOW = Path(__file__).resolve().parents[2] / ".github" / "workflows" / "ci.yml"


class ReleaseWorkflowContractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = WORKFLOW.read_text(encoding="utf-8")
        cls.ci_workflow = CI_WORKFLOW.read_text(encoding="utf-8")

    def test_fresh_go_evidence_is_explicit(self):
        for name, workflow in (("CI", self.ci_workflow), ("release", self.workflow)):
            with self.subTest(workflow=name):
                self.assertIn("go test -count=1 ./...", workflow)
                self.assertIn("go test -race -count=1 -timeout=20m ./...", workflow)
                self.assertIn("go -C tests/compatibility/go test -count=1 ./...", workflow)

    def test_downstream_preflight_precedes_irreversible_publish(self):
        preflight = self.workflow.index("\n  downstream-preflight:\n")
        publish = self.workflow.index("\n  publish:\n")
        self.assertLess(preflight, publish)
        preflight_block = self.workflow[preflight:publish]
        self.assertIn("needs: prepare", preflight_block)
        self.assertNotIn("needs: [prepare, provenance]", preflight_block)
        self.assertIn("permissions: {}", preflight_block)
        publish_block = self.workflow[publish : self.workflow.index("\n  container-push:\n")]
        self.assertIn("needs: [prepare, provenance, downstream-preflight]", publish_block)

    def test_preflight_checks_credentials_installation_and_write_permission(self):
        preflight = self.workflow[
            self.workflow.index("\n  downstream-preflight:\n") : self.workflow.index("\n  publish:\n")
        ]
        for expected in (
            "HALRO_RELEASE_APP_CLIENT_ID is empty",
            "HALRO_RELEASE_APP_PRIVATE_KEY is empty",
            "actions/create-github-app-token@",
            "repos/halro-ai/${repository}",
            ".permissions.push",
        ):
            self.assertIn(expected, preflight)


if __name__ == "__main__":
    unittest.main()
