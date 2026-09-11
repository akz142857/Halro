import re
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
FULL_SHA_ACTION = re.compile(r"^\s*-?\s*uses:\s*[^@\s]+@[0-9a-f]{40}(?:\s+#.*)?$", re.MULTILINE)
ANY_ACTION = re.compile(r"^\s*-?\s*uses:", re.MULTILINE)


def pinned_go_version() -> str:
    """The Go version go.mod pins, e.g. "1.26.6"."""
    match = re.search(r"^go (\d+\.\d+(?:\.\d+)?)$", (ROOT / "go.mod").read_text(), re.MULTILINE)
    assert match is not None, "go.mod does not pin a Go version"
    return match.group(1)


class WorkflowContractTest(unittest.TestCase):
    def test_catalog_workflow_binds_push_and_pr_to_protected_publisher(self) -> None:
        workflow = (ROOT / ".github/workflows/model-catalog-publish.yml").read_text()
        self.assertIn("token: ${{ secrets.CATALOG_PUBLISHER_TOKEN }}", workflow)
        self.assertIn("EXPECTED_PUBLISHER: ${{ vars.CATALOG_PUBLISHER_LOGIN }}", workflow)
        self.assertIn("verify_publication_gate.py", workflow)
        self.assertIn("--newer-than catalog/model-catalog-v1.json", workflow)
        self.assertEqual(len(ANY_ACTION.findall(workflow)), len(FULL_SHA_ACTION.findall(workflow)))

    def test_release_pins_every_action(self) -> None:
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        self.assertGreater(len(ANY_ACTION.findall(workflow)), 0)
        self.assertEqual(len(ANY_ACTION.findall(workflow)), len(FULL_SHA_ACTION.findall(workflow)))

    def test_release_has_one_entry_and_dispatches_exact_release_downstream(self) -> None:
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        self.assertNotRegex(workflow, r"(?m)^\s+push:\s*$")
        self.assertIn("workflow_dispatch:", workflow)
        self.assertIn("downstream-package-repositories:", workflow)
        self.assertIn("needs: [prepare, publish, container-push]", workflow)
        self.assertIn('event_type:"halro-release-published"', workflow)
        self.assertRegex(workflow, r"repositories:\s*\|\s*homebrew-tap\s+apt-repository")
        self.assertIn("commit:$commit", workflow)
        self.assertIn("permission-contents: write", workflow)

    def test_release_binds_dispatch_to_a_successful_main_ci_run(self) -> None:
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        self.assertIn("actions/workflows/ci.yml/runs", workflow)
        self.assertIn("head_sha=${GITHUB_SHA}", workflow)
        self.assertIn("status=success", workflow)
        self.assertIn("event=push", workflow)

    def test_release_repeats_local_release_only_gates(self) -> None:
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        self.assertIn("sh scripts/check-dependency-license-review.sh", workflow)
        self.assertIn("npm run typecheck", workflow)
        self.assertIn("git diff --exit-code -- internal/webui/dist", workflow)
        self.assertIn("python -m pip_audit", workflow)
        self.assertIn("npm audit --audit-level=moderate", workflow)
        self.assertIn("go -C tests/compatibility/go run golang.org/x/vuln", workflow)

    def test_sdk_dependency_inputs_are_locked_audited_and_reviewed(self) -> None:
        ci = (ROOT / ".github/workflows/ci.yml").read_text()
        requirements = (ROOT / "tests/compatibility/python/requirements.txt").read_text()
        license_gate = (ROOT / "scripts/check-dependency-license-review.sh").read_text()
        self.assertIn("--require-hashes", ci)
        self.assertIn("python -m pip_audit", ci)
        self.assertIn("npm audit --audit-level=moderate --prefix tests/compatibility/node", ci)
        self.assertIn("go -C tests/compatibility/go run golang.org/x/vuln", ci)
        self.assertIn("pip-audit==", requirements)
        self.assertIn("--hash=sha256:", requirements)
        for path in (
            "tests/compatibility/go/go.mod",
            "tests/compatibility/go/go.sum",
            "tests/compatibility/node/package.json",
            "tests/compatibility/node/package-lock.json",
            "tests/compatibility/python/requirements.in",
            "tests/compatibility/python/requirements.txt",
        ):
            self.assertIn(path, license_gate)

    def test_release_scans_and_inventories_both_container_products(self) -> None:
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        self.assertIn("image-ref: halro:release", workflow)
        self.assertIn("image-ref: halro-deadman:release", workflow)
        self.assertIn("halro-container-amd64.spdx.json", workflow)
        self.assertIn("halro-deadman-container-amd64.spdx.json", workflow)

    def test_deadman_image_contains_distribution_notices(self) -> None:
        dockerfile = (ROOT / "deploy/observability/external-probe/Dockerfile").read_text()
        self.assertIn("COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md /licenses/", dockerfile)
        self.assertIn("COPY --from=build /licenses/ /licenses/", dockerfile)

    def test_release_generates_binary_sbom_and_verifies_provenance(self) -> None:
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        self.assertIn("attestations: write", workflow)
        self.assertIn("release/sbom-binary-input", workflow)
        self.assertIn("release/halro-binaries.spdx.json", workflow)
        # The step has to exist and be pinned to a commit; which commit is
        # test_release_pins_every_action's business. Naming one here added no
        # coverage that test does not already give and turned every upgrade of
        # the action into a failure of the SBOM contract.
        self.assertRegex(workflow, r"actions/attest-build-provenance@[0-9a-f]{40}")
        self.assertIn('gh attestation verify "${artifact}"', workflow)
        self.assertIn('--source-digest "${GITHUB_SHA}"', workflow)
        self.assertIn("--source-ref refs/heads/main", workflow)

    def test_release_tag_creation_is_resume_safe(self) -> None:
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        self.assertIn('tag_commit=$(git rev-list -n 1 "refs/tags/${version}")', workflow)
        self.assertIn('tag_commit=$(git rev-list -n 1 "refs/tags/${VERSION}")', workflow)
        self.assertIn('release ${version} is already published', workflow)

    def test_release_keeps_dynamic_signed_catalog_inactive(self) -> None:
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        self.assertNotIn("MODEL_CATALOG_TRUST_ROOTS", workflow)
        self.assertNotIn("modelcatalog.ReleaseTrustRoots", workflow)

    def test_release_emits_archivable_run_evidence(self) -> None:
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        self.assertIn("tools/release/run_evidence.py create", workflow)
        self.assertIn("release-run-evidence-${{ github.run_id }}-${{ github.run_attempt }}", workflow)
        self.assertGreaterEqual(workflow.count("retention-days: 90"), 2)
        self.assertIn("release-run-evidence.json.sigstore.json", workflow)

    def test_release_archives_use_one_reproducible_timestamp(self) -> None:
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        # The release build has to run on the toolchain go.mod pins, and this
        # asserts exactly that rather than a version literal. Spelling the
        # version out here made a routine toolchain bump fail a test named after
        # timestamps, in a job the bump had no other reason to touch — and the
        # literal never checked the property that matters, which is that the two
        # files agree.
        self.assertIn(f"GOTOOLCHAIN: go{pinned_go_version()}", workflow)
        self.assertGreaterEqual(workflow.count("SOURCE_DATE_EPOCH=$(git show -s --format=%ct"), 2)
        self.assertIn("buildinfo.Date=${RELEASE_DATE}", workflow)
        self.assertIn('--mtime="@${SOURCE_DATE_EPOCH}"', workflow)
        self.assertIn("--sort=name --owner=0 --group=0 --numeric-owner", workflow)
        self.assertIn('| gzip -n >"release/halro-${GOOS}-${GOARCH}.tar.gz"', workflow)
        self.assertNotIn("buildinfo.Date=$(date -u", workflow)


if __name__ == "__main__":
    unittest.main()
