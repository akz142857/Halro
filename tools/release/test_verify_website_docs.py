import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import unittest

SPEC = importlib.util.spec_from_file_location("verify_website_docs", Path(__file__).with_name("verify_website_docs.py"))
docs = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(docs)


class WebsiteDocsTests(unittest.TestCase):
    def setUp(self):
        self.version = "v1.2.3"
        self.commit = "a" * 40
        self.manifest = b'[{"id":"chat"}]\n'
        digest = hashlib.sha256(self.manifest).hexdigest()
        self.files = {
            "src/config/distribution.ts": f"version: '{self.version}',\nreleaseCommit: '{self.commit}',".encode(),
            "src/data/contracts.meta.json": json.dumps(dict(source_ref=self.version, source_commit=self.commit, sha256=digest, source_sha256=digest)).encode(),
            "src/data/endpoint-manifests.json": self.manifest,
            "src/data/served-profile-state.json": json.dumps(dict(source_commit=self.commit)).encode(),
        }
        for path in docs.ENTRY_POINTS:
            self.files[path] = f"---\nsourceRevision: {self.commit}\ntranslatedFromRevision: {self.commit}\nverifiedAgainst: {{ release: {self.version}, commit: {self.commit} }}\n---\nCurrent {self.version}\nVERSION={self.version}\n".encode()

    def validate(self, files):
        return docs.validate_candidate(files, self.manifest, self.version, self.commit)

    def test_matching_release_and_bilingual_candidate(self):
        self.assertEqual(self.validate(self.files)["result"], "verified")

    def test_identity_requires_immutable_commit_and_version(self):
        for version, commit in (("latest", self.commit), (self.version, "main"), ("v1.2.3;bad", self.commit)):
            with self.assertRaises(ValueError):
                docs.identity(version, commit)
        docs.identity("v1.2.3-rc.1", self.commit)

    def test_package_only_bump_or_manifest_drift_is_rejected(self):
        mutations = {
            "src/config/distribution.ts": b"version: 'v1.2.2'",
            "src/data/contracts.meta.json": b'{}',
            "src/data/endpoint-manifests.json": b'[]',
            "src/data/served-profile-state.json": b'{}',
        }
        for path, replacement in mutations.items():
            with self.subTest(path=path):
                files = {**self.files, path: replacement}
                with self.assertRaises(ValueError):
                    self.validate(files)

    def test_correct_manifest_with_stale_digest_is_rejected(self):
        files = copy.deepcopy(self.files)
        meta = json.loads(files["src/data/contracts.meta.json"])
        meta["sha256"] = "0" * 64
        files["src/data/contracts.meta.json"] = json.dumps(meta).encode()
        with self.assertRaisesRegex(ValueError, "digest"):
            self.validate(files)

    def test_each_locale_and_entry_point_is_required(self):
        for path in docs.ENTRY_POINTS:
            with self.subTest(path=path):
                files = {**self.files, path: self.files[path].replace(b"sourceRevision: a", b"sourceRevision: b")}
                with self.assertRaisesRegex(ValueError, "review"):
                    self.validate(files)

    def test_old_install_pin_cannot_hide_behind_new_version_banner(self):
        path = docs.ENTRY_POINTS[1]
        for pin in (b"VERSION=v1.2.2", b"ghcr.io/akz142857/halro:v1.2.2"):
            files = {**self.files, path: self.files[path].replace(b"VERSION=v1.2.3", pin)}
            with self.assertRaisesRegex(ValueError, "stale installation"):
                self.validate(files)

    def test_english_review_cannot_lag_source(self):
        path = docs.ENTRY_POINTS[2]
        files = {**self.files, path: self.files[path].replace(b"translatedFromRevision: a", b"translatedFromRevision: b")}
        with self.assertRaisesRegex(ValueError, "translation"):
            self.validate(files)

    def pages(self):
        return {path: "<p>" + " ".join(terms) + "</p>" for path, terms in docs.public_expectations(self.version, self.commit).items()}

    def test_public_current_content_is_accepted(self):
        pages = self.pages()
        pages["/docs/guides/quickstart-install/"] = "<code>VERSION=<span>v1.2.3</span></code>"
        self.assertEqual(docs.check_public(self.version, self.commit, pages.__getitem__), [])

    def test_old_body_with_current_header_is_not_accepted(self):
        pages = self.pages()
        pages["/docs/"] = "<header>v1.2.3</header><p>v0.9.1 正式文档</p>"
        self.assertTrue(docs.check_public(self.version, self.commit, pages.__getitem__))

    def test_same_version_api_from_wrong_commit_is_not_accepted(self):
        pages = self.pages()
        pages["/en/docs/api/chat-completions/"] = "v1.2.3 bbbbbbbbbbbb"
        self.assertTrue(docs.check_public(self.version, self.commit, pages.__getitem__))

    def test_hidden_markers_and_network_errors_are_not_success(self):
        pages = self.pages()
        pages["/en/docs/"] = "<script>v1.2.3 documentation</script>old content"
        self.assertTrue(docs.check_public(self.version, self.commit, pages.__getitem__))
        def unavailable(_):
            raise TimeoutError("connection timeout")
        self.assertEqual(len(docs.check_public(self.version, self.commit, unavailable)), 6)


if __name__ == "__main__":
    unittest.main()
