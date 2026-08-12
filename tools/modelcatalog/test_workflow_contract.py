import re
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
FULL_SHA_ACTION = re.compile(r"^\s*-?\s*uses:\s*[^@\s]+@[0-9a-f]{40}(?:\s+#.*)?$", re.MULTILINE)
ANY_ACTION = re.compile(r"^\s*-?\s*uses:", re.MULTILINE)


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
        self.assertIn("actions/attest-build-provenance@977bb373ede98d70efdf65b84cb5f73e068dcc2a", workflow)
        self.assertIn('gh attestation verify "${artifact}"', workflow)

    def test_release_keeps_dynamic_signed_catalog_inactive(self) -> None:
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        self.assertNotIn("MODEL_CATALOG_TRUST_ROOTS", workflow)
        self.assertNotIn("modelcatalog.ReleaseTrustRoots", workflow)

    def test_release_refuses_to_auto_create_an_unprotected_environment(self) -> None:
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        governance = workflow.split("  release-governance:\n", 1)[1].split("\n  publish:\n", 1)[0]
        self.assertIn("actions: read", governance)
        self.assertIn("contents: read", governance)
        self.assertNotIn("deployments: read", governance)
        self.assertIn('gh api "repos/${GITHUB_REPOSITORY}/environments/v1-release"', governance)
        self.assertIn("tools/release/verify_environment.py", governance)
        self.assertIn("needs: [provenance, release-governance]", workflow)

    def test_release_emits_archivable_run_evidence(self) -> None:
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        self.assertIn("tools/release/run_evidence.py create", workflow)
        self.assertIn("release-run-evidence-${{ github.run_id }}-${{ github.run_attempt }}", workflow)
        self.assertGreaterEqual(workflow.count("retention-days: 90"), 2)
        self.assertIn("release-run-evidence.json.sigstore.json", workflow)

    def test_release_archives_use_one_reproducible_timestamp(self) -> None:
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        self.assertIn("GOTOOLCHAIN: go1.26.5", workflow)
        self.assertGreaterEqual(workflow.count("SOURCE_DATE_EPOCH=$(git show -s --format=%ct"), 2)
        self.assertIn("buildinfo.Date=${RELEASE_DATE}", workflow)
        self.assertIn('--mtime="@${SOURCE_DATE_EPOCH}"', workflow)
        self.assertIn("--sort=name --owner=0 --group=0 --numeric-owner", workflow)
        self.assertIn('| gzip -n >"release/halro-${GOOS}-${GOARCH}.tar.gz"', workflow)
        self.assertNotIn("buildinfo.Date=$(date -u", workflow)


if __name__ == "__main__":
    unittest.main()
