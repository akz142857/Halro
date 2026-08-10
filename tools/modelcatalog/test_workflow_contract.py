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

    def test_release_privileged_jobs_pin_every_action(self) -> None:
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        privileged = workflow.split("\n  provenance:\n", 1)[1]
        self.assertGreater(len(ANY_ACTION.findall(privileged)), 0)
        self.assertEqual(len(ANY_ACTION.findall(privileged)), len(FULL_SHA_ACTION.findall(privileged)))


if __name__ == "__main__":
    unittest.main()
