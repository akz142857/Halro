import unittest

from tools.modelcatalog.verify_publication_gate import validate_publication_gate


SHA = "a" * 40


def valid_values() -> dict[str, object]:
    return {
        "expected_publisher": "halro-catalog-publisher",
        "actual_publisher": "halro-catalog-publisher",
        "expected_repository": "claycosmos/halro",
        "actual_head_repository": "claycosmos/halro",
        "head_ref": "catalog/publish-1234-1",
        "pr_base_sha": SHA,
        "current_main_sha": SHA,
        "changed_files": ["catalog/model-catalog-v1.json"],
    }


class PublicationGateTest(unittest.TestCase):
    def test_accepts_protected_publisher_on_current_main(self) -> None:
        validate_publication_gate(**valid_values())

    def test_rejects_direct_pr_author(self) -> None:
        values = valid_values()
        values["actual_publisher"] = "catalog-author"
        with self.assertRaisesRegex(ValueError, "protected publisher"):
            validate_publication_gate(**values)

    def test_rejects_fork_or_spoofed_branch(self) -> None:
        for key, value in (
            ("actual_head_repository", "someone/fork"),
            ("head_ref", "catalog/publish-manual"),
        ):
            with self.subTest(key=key):
                values = valid_values()
                values[key] = value
                with self.assertRaises(ValueError):
                    validate_publication_gate(**values)

    def test_rejects_stale_base(self) -> None:
        values = valid_values()
        values["current_main_sha"] = "b" * 40
        with self.assertRaisesRegex(ValueError, "not based on current main"):
            validate_publication_gate(**values)

    def test_rejects_extra_or_missing_paths(self) -> None:
        for paths in ([], ["catalog/model-catalog-v1.json", "README.md"]):
            with self.subTest(paths=paths):
                values = valid_values()
                values["changed_files"] = paths
                with self.assertRaisesRegex(ValueError, "only the production catalog"):
                    validate_publication_gate(**values)


if __name__ == "__main__":
    unittest.main()
