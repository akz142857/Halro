#!/usr/bin/env python3

from pathlib import Path
import unittest


WORKFLOW = Path(__file__).resolve().parents[2] / ".github" / "workflows" / "release.yml"
GHCR_WORKFLOW = Path(__file__).resolve().parents[2] / ".github" / "workflows" / "publish-ghcr.yml"
CI_WORKFLOW = Path(__file__).resolve().parents[2] / ".github" / "workflows" / "ci.yml"


class ReleaseWorkflowContractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = WORKFLOW.read_text(encoding="utf-8")
        cls.ghcr_workflow = GHCR_WORKFLOW.read_text(encoding="utf-8")
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
        self.assertIn("inputs.publish_packages == true", preflight_block)
        publish_block = self.workflow[publish : self.workflow.index("\n  container-push:\n")]
        self.assertIn("needs: [prepare, provenance, downstream-preflight]", publish_block)
        self.assertIn("needs.provenance.result == 'success'", publish_block)
        self.assertIn("needs.downstream-preflight.result == 'success'", publish_block)
        self.assertIn("needs.downstream-preflight.result == 'skipped'", publish_block)

    def test_github_only_release_is_explicit_and_does_not_dispatch_packages(self):
        self.assertIn("publish_packages:", self.workflow)
        self.assertIn('description: "Dispatch the published release to Homebrew and APT"', self.workflow)
        downstream = self.workflow[self.workflow.index("\n  downstream-package-repositories:\n") :]
        self.assertIn("inputs.publish_packages == true", downstream)

    def test_github_only_release_still_pushes_containers(self):
        container_push = self.workflow[
            self.workflow.index("\n  container-push:\n") : self.workflow.index("\n  downstream-package-repositories:\n")
        ]
        self.assertIn("always()", container_push)
        self.assertIn("needs.publish.result == 'success'", container_push)

    def test_ghcr_recovery_uses_only_verified_published_assets(self):
        for expected in (
            "workflow_dispatch:",
            "release_commit:",
            'GITHUB_REF}" != "refs/heads/${DEFAULT_BRANCH}',
            'tag_commit=$(git rev-list -n 1 "refs/tags/${VERSION}")',
            "gh release view",
            "gh release download",
            "sha256sum --check --ignore-missing checksums.txt",
            'gh attestation verify "${artifact}"',
            "cosign verify-blob",
            "packages: write",
            'docker buildx imagetools create -t "ghcr.io/${owner}/${name}:${VERSION}"',
        ):
            self.assertIn(expected, self.ghcr_workflow)
        self.assertNotIn("docker build ", self.ghcr_workflow)
        self.assertNotIn("docker buildx build", self.ghcr_workflow)

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
