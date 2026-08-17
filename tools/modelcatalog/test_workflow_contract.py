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
