# Release Environment and run-evidence verification

Status: repository controls implemented; no rc.2 run or publication is claimed
by this document.

This procedure closes the repository side of R-01 and R-04p. The remaining
acceptance evidence necessarily comes from GitHub: a protected Environment and
one retained release workflow run. Do not replace those observations with a
local test result or a hand-written digest list.

## 1. Configure the release Environment before creating a tag

Create `v1-release` in the repository settings. Configure at least one required
reviewer and enable **Prevent self-review**. The reviewer who approves the job
must be independent from the operator who installs the evidence secret.

Do this before the tag: `release-governance` reads the Environment's protection
rules and fails the run when they are missing, so a tag pushed first cannot
publish.

The evidence secret is installed later, during the approval pause, and not here.
It has to name the SHA-256 of the two container archives, which are not
byte-reproducible, so it cannot be completed until a run has built them —
`docs/guides/releasing.md` sets out the full order. This section covers only what
that secret must look like once it exists.

Install `M11_RELEASE_EVIDENCE_JSON` as an **Environment secret**, never as a
repository secret. Confirm the scope without printing its value:

```sh
gh secret list --env v1-release
gh secret list
```

The first command must list `M11_RELEASE_EVIDENCE_JSON`; the second must not.
Verify the protection response with the same parser used by the release
workflow:

```sh
response=$(mktemp)
trap 'rm -f "$response"' EXIT
gh api repos/akz142857/Halro/environments/v1-release >"$response"
python3 tools/release/verify_environment.py --name v1-release "$response"
```

The `release-governance` job performs this check before `publish` is scheduled.
If the Environment is missing or unprotected, `publish` never references it;
GitHub therefore cannot silently auto-create an unprotected environment for
this workflow.

## 2. Evidence emitted by every release run

After creating and signing the release assets, the `provenance` job emits two
90-day workflow artifacts:

- `release-assets`, containing the archives, both SBOMs, checksums, and their
  Sigstore bundles;
- `release-run-evidence-RUN_ID-ATTEMPT`, containing a signed
  `release-run-evidence.json`.

The manifest binds the workflow run ID and attempt, exact commit, tag ref,
workflow identity, and the name, size, and SHA-256 digest of every file in
`release-assets`. It is generated only after all release-file signatures exist,
so it also covers those bundles. It is not copied into the release directory,
which avoids a self-referential digest.

`publish` is scheduled once these exist and then waits for the Environment
reviewer. That pause is when the M11 bundle is completed against this run's
`release-assets` and installed as the Environment secret; approving before it is
installed fails the job on an empty secret, recoverable with
`gh run rerun --failed` while the 90-day artifacts survive. The full order is in
`docs/guides/releasing.md`.

## 3. Preserve the run before its retention window expires

Install and authenticate `gh`, and install `jq`, `cosign`, `python3`, and
`shasum` on the archival host before running this step.

Do not delete the run. Download its complete logs and both artifact sets into
restricted, immutable evidence storage:

```sh
./scripts/archive-release-run.sh RUN_ID \
  /secure/release-evidence/v1.0.0-rc.2-RUN_ID
```

The script is read-only against GitHub. It refuses to overwrite an existing
archive, verifies that run ID/attempt/commit match GitHub metadata, verifies the
manifest's Sigstore identity, recomputes every recorded artifact digest, saves
the complete workflow log, and writes `archive-sha256.txt` over the resulting
archive.

Record the secure-storage locator, run URL, run ID, attempt, commit, tag, and
`archive-sha256.txt` digest in the release approval record. Do not commit the
M11 evidence bundle, Provider evidence, full run logs, or restricted archive to
this public repository.

## Acceptance boundary

Repository tests prove only that the workflow fails closed and that generated
manifests detect missing, extra, or modified files. R-01/R-04p become verified
only after the live GitHub Environment response passes, the intended rc run is
retained, and the archived manifest and logs pass the procedure above. Creating
a Release is a later, separate operation.
