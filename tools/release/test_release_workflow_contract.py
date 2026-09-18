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

    def test_every_published_artifact_shape_is_checksummed_signed_and_verified(self):
        # build_deb.sh emits halro_<version>-1_<arch>.deb and
        # halro-deadman_<version>-1_<arch>.deb. Only the second one starts with
        # "halro-", so a list written as "halro-*" alone covers the deadman
        # package and silently drops the main one: it reaches the release
        # unchecksummed and unsigned, and the verify job — reading the same
        # list — never notices. Every list that names artifacts must name both
        # shapes.
        checksums = self.workflow[
            self.workflow.index("- name: Generate checksums") : self.workflow.index("- name: Install Cosign")
        ]
        self.assertIn("sha256sum halro-* halro_*.deb halro.spdx.json > checksums.txt", checksums)

        signing = self.workflow[
            self.workflow.index("- name: Keyless sign release blobs") : self.workflow.index(
                "- name: Generate and sign release-run evidence manifest"
            )
        ]
        self.assertIn("for artifact in halro-* halro_*.deb halro.spdx.json checksums.txt; do", signing)

        verify = self.workflow[
            self.workflow.index("- name: Verify release blobs") : self.workflow.index("- name: Create the release tag")
        ]
        self.assertIn(
            "for artifact in release/halro-* release/halro_*.deb release/halro.spdx.json release/checksums.txt; do",
            verify,
        )
        self.assertIn("(cd release && sha256sum --check checksums.txt)", verify)

    def test_both_released_binaries_carry_the_same_build_identity(self):
        # The dead-man ships in the same archive and runs outside Halro's
        # failure domain; a probe that cannot say which build it is cannot be
        # tied to the release it came from.
        build = self.workflow[
            self.workflow.index("package_dir=\"release/halro-${GOOS}-${GOARCH}\"") : self.workflow.index(
                "cp deploy/observability/external-probe/config.example.yaml"
            )
        ]
        self.assertEqual(build.count("internal/buildinfo.Version=${RELEASE_VERSION}"), 2)
        self.assertEqual(build.count("internal/buildinfo.Commit=${RELEASE_COMMIT}"), 2)
        self.assertEqual(build.count("internal/buildinfo.Date=${RELEASE_DATE}"), 2)

    def test_every_fuzz_target_in_the_tree_is_listed_in_ci(self):
        # ci.yml already fails when a listed target no longer exists. The other
        # direction had no guard: `go test -fuzz` exits 0 when its pattern
        # matches nothing, so a target added under a name nobody listed is
        # never fuzzed and the job stays green either way.
        # Scoped to internal/, which is what the ci.yml fuzz list covers.
        # tests/compatibility/go is a separate module that job never fuzzes.
        internal = Path(__file__).resolve().parents[2] / "internal"
        declared = set()
        for path in internal.rglob("*_test.go"):
            for line in path.read_text(encoding="utf-8").splitlines():
                if line.startswith("func Fuzz") and "(" in line:
                    declared.add(line[len("func ") : line.index("(")])
        self.assertTrue(declared, "no fuzz targets found; this check would pass vacuously")
        unlisted = sorted(name for name in declared if f":{name} " not in self.ci_workflow and f":{name}\n" not in self.ci_workflow)
        self.assertEqual(unlisted, [], f"fuzz targets missing from the ci.yml list: {unlisted}")


if __name__ == "__main__":
    unittest.main()
