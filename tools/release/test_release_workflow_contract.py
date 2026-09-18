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
RELEASE_NOTES = Path(__file__).resolve().parents[2] / "tools" / "release" / "release_notes.sh"
CHANGELOG = Path(__file__).resolve().parents[2] / "CHANGELOG.md"


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
        # The three copies name the variable differently; what has to hold is the
        # idiom, not the name. verify-release.sh is a copy of the script that
        # runs in the private control plane, so this assertion says nothing
        # about production on its own — see packaging/apt-repository/README.md.
        variables = {
            "release workflow": "debian_upstream",
            "package builder": "debian_upstream",
            "release verifier": "expected_package_version",
        }
        for name, source in sources.items():
            with self.subTest(source=name):
                # In Bash 5, using an unescaped tilde as the replacement in
                # ${value/-/~} expands it to $HOME even inside double quotes.
                self.assertNotIn("/-/~}", source)
                variable = variables[name]
                self.assertIn(f"${{{variable}%%-*}}~${{{variable}#*-}}", source)

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

    def test_release_notes_come_from_the_changelog(self):
        # --generate-notes writes the merged-pull-request list, which describes
        # how the work arrived rather than what the release is. v0.8.4 published
        # with its eleven dependency bumps above its one substantive change.
        publish = self.workflow[
            self.workflow.index("- name: Publish GitHub release") : self.workflow.index("\n  container-push:\n")
        ]
        self.assertNotIn("--generate-notes", publish)
        self.assertIn('tools/release/release_notes.sh "${VERSION}"', publish)
        self.assertIn('--notes-file "${RUNNER_TEMP}/release-notes.md"', publish)

    def test_release_notes_render_the_requested_section_and_refuse_a_missing_one(self):
        changelog = (
            "# Changelog\n\n"
            "## [Unreleased]\n\n"
            "## [1.2.3] - 2026-01-01\n\n"
            "### Fixed\n\n- the thing this release fixed\n\n"
            "## [1.2.2] - 2025-12-01\n\n"
            "### Fixed\n\n- an older release nobody asked for\n"
        )
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "CHANGELOG.md"
            path.write_text(changelog, encoding="utf-8")

            rendered = subprocess.run(
                [str(RELEASE_NOTES), "v1.2.3", str(path)],
                capture_output=True,
                text=True,
                check=True,
                env={**os.environ, "GITHUB_REPOSITORY": "owner/Halro"},
            ).stdout
            self.assertIn("- the thing this release fixed", rendered)
            self.assertNotIn("an older release nobody asked for", rendered)
            self.assertNotIn("## [1.2.2]", rendered)
            self.assertIn("ghcr.io/owner/halro:v1.2.3", rendered)
            self.assertIn("ghcr.io/owner/halro-deadman:v1.2.3", rendered)

            # A version with no section must not publish empty notes under an
            # immutable tag that already exists by the time this runs.
            missing = subprocess.run(
                [str(RELEASE_NOTES), "v9.9.9", str(path)],
                capture_output=True,
                text=True,
            )
            self.assertNotEqual(missing.returncode, 0)
            self.assertIn("no content under", missing.stderr)

            # An empty section is the same failure as an absent one.
            empty = Path(directory) / "EMPTY.md"
            empty.write_text("## [1.2.3] - 2026-01-01\n\n## [1.2.2] - 2025-12-01\n\n- older\n", encoding="utf-8")
            blank = subprocess.run(
                [str(RELEASE_NOTES), "v1.2.3", str(empty)],
                capture_output=True,
                text=True,
            )
            self.assertNotEqual(blank.returncode, 0)

    def test_the_published_changelog_section_renders_for_the_current_version(self):
        # The renderer is only as good as the section it reads: a heading style
        # the extractor cannot match would fail at publish time, after the tag.
        changelog = CHANGELOG.read_text(encoding="utf-8")
        latest = re.search(r"^## \[(\d+\.\d+\.\d+)\]", changelog, re.MULTILINE).group(1)
        rendered = subprocess.run(
            [str(RELEASE_NOTES), f"v{latest}", str(CHANGELOG)],
            capture_output=True,
            text=True,
            check=True,
        ).stdout
        self.assertIn("## Install", rendered)
        self.assertNotIn("## [", rendered)
        self.assertGreater(len(rendered.splitlines()), 20)

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

    @staticmethod
    def _bash_has_associative_arrays():
        probe = subprocess.run(["bash", "-c", "declare -A x 2>/dev/null"], capture_output=True)
        return probe.returncode == 0

    @unittest.skipUnless(shutil.which("sha256sum"), "sha256sum is required by the release verifier")
    def test_apt_verifier_refuses_an_incomplete_or_unchecksummed_release(self):
        # verify-release.sh here is a copy of the script that runs in the private
        # halro-ai/apt-repository. Running the copy is the only check available:
        # the control plane is private, so nothing can compare the two
        # automatically. packaging/apt-repository/README.md says so out loud.
        if not self._bash_has_associative_arrays():
            self.skipTest("the verifier's package matrix needs bash 4+; macOS ships 3.2")
        commit = "a" * 40
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            binaries = root / "bin"
            binaries.mkdir()

            # gh: resolves the tag, then materialises a release whose shape the
            # environment chooses — complete, missing one package, or with one
            # package left out of checksums.txt.
            gh = binaries / "gh"
            gh.write_text(
                """#!/usr/bin/env bash
set -eu
if [ "$1" = api ]; then
  printf '%s\\n' "${MOCK_COMMIT}"
  exit 0
fi
if [ "$1" = attestation ]; then
  exit 0
fi
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
      if [ "${MOCK_DROP_PACKAGE:-}" = "$package" ]; then
        continue
      fi
      printf '%s\\n' "$package" >"$output/$package"
      : >"$output/$package.sigstore.json"
    done
  done
  (cd "$output" && sha256sum *.deb >checksums.txt)
  if [ -n "${MOCK_UNLISTED_PACKAGE:-}" ]; then
    grep -v -- "${MOCK_UNLISTED_PACKAGE}" "$output/checksums.txt" >"$output/checksums.tmp"
    mv "$output/checksums.tmp" "$output/checksums.txt"
  fi
  : >"$output/checksums.txt.sigstore.json"
fi
""",
                encoding="utf-8",
            )
            gh.chmod(0o755)
            for name in ("cosign",):
                stub = binaries / name
                stub.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
                stub.chmod(0o755)
            # dpkg-deb answers from the filename, which is what the release
            # workflow builds the name from in the first place.
            dpkg = binaries / "dpkg-deb"
            dpkg.write_text(
                """#!/usr/bin/env bash
set -eu
# dpkg-deb --field <archive> <field-name>
package=$(basename "$2" .deb)
field=$3
name=${package%%_*}
rest=${package#*_}
version=${rest%%_*}
architecture=${rest#*_}
case "$field" in
  Package) printf '%s\\n' "$name" ;;
  Version) printf '%s\\n' "$version" ;;
  Architecture) printf '%s\\n' "$architecture" ;;
esac
""",
                encoding="utf-8",
            )
            dpkg.chmod(0o755)

            version = "v1.2.3-rc.1"
            environment = {
                **os.environ,
                "PATH": f"{binaries}{os.pathsep}{os.environ['PATH']}",
                "MOCK_VERSION": version,
                "MOCK_COMMIT": commit,
            }

            def run(name, extra=None):
                return subprocess.run(
                    ["bash", str(VERIFY_RELEASE), version, commit, str(root / name)],
                    check=False, capture_output=True, text=True, env={**environment, **(extra or {})},
                )

            # A prerelease tag must accept packages named 1.2.3~rc.1-1. Under
            # Bash 5 the old derivation produced 1.2.3/home/runnerrc.1-1 here and
            # rejected every one of them.
            complete = run("complete")
            self.assertEqual(complete.returncode, 0, complete.stderr)

            missing = run("missing", {"MOCK_DROP_PACKAGE": "halro_1.2.3~rc.1-1_arm64.deb"})
            self.assertNotEqual(missing.returncode, 0)
            self.assertIn("halro and halro-deadman for amd64 and arm64", missing.stderr)

            # sha256sum --check --ignore-missing passes a package it was never
            # told about; only the membership assertion refuses it.
            unlisted = run("unlisted", {"MOCK_UNLISTED_PACKAGE": "halro_1.2.3~rc.1-1_amd64.deb"})
            self.assertNotEqual(unlisted.returncode, 0)
            self.assertIn("is not listed in checksums.txt", unlisted.stderr)

            wrong_commit = run("wrong-commit", {"MOCK_COMMIT": "b" * 40})
            self.assertNotEqual(wrong_commit.returncode, 0)
            self.assertIn("resolves to", wrong_commit.stderr)


if __name__ == "__main__":
    unittest.main()
