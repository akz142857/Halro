import unittest

from verify_fault_matrix import evaluate_tests, source_fingerprints


class FaultMatrixResultTest(unittest.TestCase):
    def test_freeze_includes_embedded_template_reference_and_ui(self):
        captured = source_fingerprints()
        for path in ("internal/config/default.yaml", "configs/config.example.yaml",
                     "tools/memory-state/source_fingerprints.py",
                     "docs/runbooks/configuration-stale.md",
                     "docs/runbooks/m11-kms-key-lifecycle.md",
                     "docs/runbooks/m11-kms-disaster-recovery.md",
                     "docs/runbooks/gateway-key-compromise.md",
                     "docs/runbooks/file-master-key-rotation.md"):
            self.assertIn(path, captured)
        self.assertTrue(any(path.startswith("internal/webui/dist/") for path in captured))
    def test_successful_package_without_test_is_not_evidence(self):
        self.assertEqual(evaluate_tests([], "p", {"TestRequired"}, 0),
                         {"TestRequired": "not_executed"})

    def test_subtest_and_other_package_cannot_substitute_for_required_test(self):
        events = [{"Package": "p", "Test": "TestRequired/sub", "Action": "pass"},
                  {"Package": "other", "Test": "TestRequired", "Action": "pass"}]
        self.assertEqual(evaluate_tests(events, "p", {"TestRequired"}, 0),
                         {"TestRequired": "not_executed"})

    def test_skip_and_package_failure_never_become_pass(self):
        events = [{"Package": "p", "Test": "TestA", "Action": "skip"},
                  {"Package": "p", "Test": "TestB", "Action": "pass"}]
        self.assertEqual(evaluate_tests(events, "p", {"TestA", "TestB"}, 1),
                         {"TestA": "skip", "TestB": "package_failed"})

    def test_exact_executed_test_can_pass(self):
        events = [{"Package": "p", "Test": "TestA", "Action": "run"},
                  {"Package": "p", "Test": "TestA", "Action": "pass"}]
        self.assertEqual(evaluate_tests(events, "p", {"TestA"}, 0), {"TestA": "pass"})


if __name__ == "__main__":
    unittest.main()
