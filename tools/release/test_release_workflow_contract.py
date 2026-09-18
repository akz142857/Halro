#!/usr/bin/env python3

import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest


WORKFLOW = Path(__file__).resolve().parents[2] / ".github" / "workflows" / "release.yml"
GHCR_WORKFLOW = Path(__file__).resolve().parents[2] / ".github" / "workflows" / "publish-ghcr.yml"
CI_WORKFLOW = Path(__file__).resolve().parents[2] / ".github" / "workflows" / "ci.yml"
VERIFY_RELEASE = Path(__file__).resolve().parents[2] / "packaging" / "apt-repository" / "scripts" / "verify-release.sh"
BUILD_DEB = Path(__file__).resolve().parents[2] / "tools" / "release" / "build_deb.sh"


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

        package_build = self.workflow[
            self.workflow.index("- name: Build Debian packages from the released Linux binaries") :
            self.workflow.index("name: halro-debian")
        ]
        self.assertIn("for package_name in halro halro-deadman; do", package_build)
        self.assertIn("for arch in amd64 arm64; do", package_build)
        self.assertIn('test -f "release/${package_name}_${package_version}_${arch}.deb"', package_build)

    def test_debian_prerelease_conversion_cannot_expand_home(self):
        sources = {
            "release workflow": self.workflow,
            "package builder": BUILD_DEB.read_text(encoding="utf-8"),
            "release verifier": VERIFY_RELEASE.read_text(encoding="utf-8"),
        }
        for name, source in sources.items():
            with self.subTest(source=name):
                # In Bash 5, using an unescaped tilde as the replacement in
                # ${value/-/~} expands it to $HOME even inside double quotes.
                self.assertNotIn("/-/~}", source)
                self.assertIn('~${debian_upstream#*-}', source)

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
                    package = "./" + path.parent.relative_to(WORKFLOW.parents[2]).as_posix()
                    declared.add((package, line[len("func ") : line.index("(")]))
        self.assertTrue(declared, "no fuzz targets found; this check would pass vacuously")

        loop = re.search(
            r"(?ms)^\s+for entry in \\\n(?P<entries>.*?)^\s+do\s*$",
            self.ci_workflow,
        )
        self.assertIsNotNone(loop, "ci.yml no longer contains the fuzz target loop")
        listed = set()
        for line in loop.group("entries").splitlines():
            match = re.fullmatch(r"\s+(\./internal/[^:\s]+):(Fuzz\w+)(?:\s+\\)?", line)
            self.assertIsNotNone(match, f"unparseable fuzz target line: {line!r}")
            listed.add((match.group(1), match.group(2)))
        self.assertEqual(listed, declared, f"ci.yml fuzz target set differs: missing={sorted(declared - listed)}, stale={sorted(listed - declared)}")

    @unittest.skipUnless(shutil.which("sha256sum"), "sha256sum is required by the release verifier")
    def test_apt_verifier_requires_the_complete_product_architecture_matrix(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            binaries = root / "bin"
            binaries.mkdir()
            gh = binaries / "gh"
            gh.write_text(
                """#!/usr/bin/env bash
set -eu
if [ "$1" = release ] && [ "$2" = download ]; then
  shift 2
  output=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --dir) output=$2; shift 2 ;;
      *) shift ;;
    esac
  done
  mkdir -p "$output"
  version=${MOCK_VERSION#v}
  if [[ "$version" == *-* ]]; then
    version="${version%%-*}~${version#*-}"
  fi
  package_version=${version}-1
  for product in halro halro-deadman; do
    for architecture in amd64 arm64; do
      package=${product}_${package_version}_${architecture}.deb
      if [ "${MOCK_COMPLETE:-0}" != 1 ] && [ "$package" = "halro_${package_version}_arm64.deb" ]; then
        continue
      fi
      printf '%s\\n' "$package" >"$output/$package"
      : >"$output/$package.sigstore.json"
    done
  done
  (cd "$output" && sha256sum *.deb >checksums.txt)
  : >"$output/checksums.txt.sigstore.json"
fi
""",
                encoding="utf-8",
            )
            gh.chmod(0o755)
            cosign = binaries / "cosign"
            cosign.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            cosign.chmod(0o755)
            environment = {**os.environ, "PATH": f"{binaries}{os.pathsep}{os.environ['PATH']}", "MOCK_VERSION": "v1.2.3-rc.1"}

            incomplete = subprocess.run(
                ["bash", str(VERIFY_RELEASE), "v1.2.3-rc.1", str(root / "incomplete")],
                check=False, capture_output=True, text=True, env=environment,
            )
            self.assertNotEqual(incomplete.returncode, 0)
            self.assertIn("missing halro_1.2.3~rc.1-1_arm64.deb", incomplete.stderr)

            complete = subprocess.run(
                ["bash", str(VERIFY_RELEASE), "v1.2.3-rc.1", str(root / "complete")],
                check=False, capture_output=True, text=True, env={**environment, "MOCK_COMPLETE": "1"},
            )
            self.assertEqual(complete.returncode, 0, complete.stderr)


if __name__ == "__main__":
    unittest.main()
