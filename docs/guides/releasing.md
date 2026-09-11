# Release Process

> **Status.** Two things are described here, and until now the document did not
> separate them. The **v0.x line** is what `.github/workflows/release.yml`
> actually does today: twelve jobs — `prepare`, `quality`, `sdk-compatibility`,
> `stress`, `web`, `binaries`, `container`, `debian-packages`, `provenance`,
> `publish`, `container-push`, `downstream-package-repositories` — with the
> publication preconditions being `prepare`'s CHANGELOG section check, a
> successful ordinary `ci.yml` push run for the exact `main` commit, and a
> manual `workflow_dispatch` from `main`. There is no environment approval, no `release-governance` preflight,
> no signed-tag requirement, and no M11 evidence verification in that workflow.
>
> The **1.0.0 governance gates** — the `v1-release` environment with required
> reviewers, the `release-governance` preflight, the annotated-signature
> requirement, and `M11_RELEASE_EVIDENCE_JSON` verification — are a target, not
> the current pipeline. They were retired for the v0.x line by owner decision on
> 2026-08-16, recorded in `docs/verification/release-assessment.md`: the v0.x
> line has no release candidates or reviewer-approval gate, and the
> go/no-go is the owner's, made against a filled assessment record.
>
> Sections describing the 1.0.0 gates are marked **[1.0.0 target]**. Do not
> follow them when tagging a v0.x release: the approval they tell you to wait
> for will not appear, and the evidence bundle they tell you to prepare is not
> read by anything.

## What the v0.x pipeline does

Running `release` through `workflow_dispatch` on `main` is the only supported
entry. It runs the full matrix, creates the annotated version tag only after all
gates pass, publishes the immutable release and container images, then dispatches
that exact version and full commit to both package repositories. The
gates that genuinely hold are the exact-commit ordinary CI proof plus the ones
inside those jobs: `go test`, `go test -race`, `go vet`, `govulncheck`, the
ordinary-CI fuzz targets, the official-SDK compatibility and dependency audits,
the SSE stress run, the frontend suite/typecheck/bundle-drift check, Trivy on
both containers, reproducible packaging, source/binary/container SBOMs, cosign
signatures, and `gh attestation verify` before publication. `prepare` refuses to
start the matrix unless `CHANGELOG.md` carries a section for the version being
tagged.

What is **not** enforced anywhere: that the CHANGELOG section is complete, that
the pre-release assessment in `docs/verification/assessments/` has been filled
in, or that anyone other than the tagger has looked at the release. Those are
procedure, and the procedure is `docs/verification/release-assessment.md`.

The release workflow builds the embedded React UI once and cross-compiles
static Halro binaries for Linux and macOS on amd64 and arm64. Windows is not
a v1 target because the exclusive data-directory lock currently uses Unix
`flock` semantics.

Every release run produces:

- `halro-deadman` in every supported binary archive together with its
  versioned config/event schemas, receiver contract, and systemd unit;
- the main Halro example configuration as `halro.config.example.yaml` in every
  supported binary archive, separate from the dead-man configuration;
- a non-root `halro-deadman-container.tar.gz` image archive built from
  digest-pinned base images;

- four version-stamped, stripped binary archives, each containing `LICENSE`,
  `NOTICE`, `THIRD_PARTY_NOTICES.md`, and `README.md`;
- `halro` and `halro-deadman` Debian packages for Linux amd64 and arm64, built
  from those already-gated Linux archives rather than by recompiling binaries;
- a non-root distroless container image exported as `halro-container.tar.gz`;
- an SPDX JSON source/dependency SBOM, a separate SPDX JSON SBOM generated
  from the released binaries, and per-architecture image SBOMs for both
  container products;
- SHA-256 checksums;
- a Sigstore keyless bundle for each binary archive, the SBOM, and checksum file;
- a GitHub build-provenance attestation for every archive and SBOM, verified
  with `gh attestation verify` before publication;
- workflow artifacts, an annotated version tag, and an immutable GitHub Release.

The GitHub Release is the source of truth for downstream package channels.
`halro-ai/homebrew-tap` pins its Formula URLs and SHA-256 values to those immutable
archives. The APT publisher imports only the attested `.deb` assets into the
signed repository at `packages.halro.ai`; neither downstream system may rebuild
or replace a binary. A package channel is advertised on `halro.ai` only after a
clean-host installation smoke passes. Failure in a downstream publisher delays
that channel and never retracts or mutates the Git tag or GitHub Release.

The v0.x path is exclusively `workflow_dispatch` on `main`. Fulcio therefore puts
`release.yml@refs/heads/main` in those artifacts' certificate identity; the
checksums and GitHub provenance attestation bind each blob to the exact commit.

## One-time package automation setup

Create one GitHub App installed on `akz142857/Halro`,
`halro-ai/homebrew-tap`, `halro-ai/apt-repository`, and
`akz142857/Halro-website`. Grant repository Contents, Issues, and Pull requests
read/write. Store its client ID as `HALRO_RELEASE_APP_CLIENT_ID` and private key
as `HALRO_RELEASE_APP_PRIVATE_KEY` in every participating repository. The App
token is required: pull requests created by the default workflow token do not
start the downstream validation workflows.

In `halro-ai/apt-repository`, create the protected `apt-production` Environment.
Store an unencrypted, dedicated online OpenPGP archive-signing private key as the
Environment secret `HALRO_APT_ARCHIVE_SIGNING_KEY`; store its uppercase full
fingerprint as `HALRO_APT_ARCHIVE_SIGNING_FINGERPRINT`. Keep the private key out
of every repository and log. The cluster's existing `ghcr-credentials` pull
secret must be authorized to read `halro-ai/apt-repository`; the snapshot image
remains private while `packages.halro.ai/apt` is the public package surface.

The website package ingress and automation workflows must already be present on
the default branch because `repository_dispatch` only loads workflows there.
Once those one-time conditions are met, a release operator does exactly one
thing: run `.github/workflows/release.yml` on `main` with the new version and
`dry_run=false`. Homebrew and APT update through reviewed automation branches;
APT switches the GitOps image by digest, executes clean-host Debian/Ubuntu
amd64/arm64 acceptance, waits for the Homebrew Formula, and only then updates
the public install page.

## v0.x package release operator checklist

Use this checklist for `v0.8.0` and later v0.x releases. Replace `v0.8.0` in
the examples, but do not change the repository names or run a release from a
branch other than `main`.

### 1. Decide the release contents and exact commit

Merge every intended change before starting the release. A local working tree,
an unmerged pull request, or a commit on another branch is not part of the
release. Add a complete `## [0.8.0]` section to `CHANGELOG.md` and fill a
release assessment under `docs/verification/assessments/`. The workflow checks
that the changelog section exists and that ordinary CI passed for this exact
`main` commit; the owner remains responsible for the
assessment being complete and for recording any explicitly waived external
acceptance, such as a real-Provider smoke.

Fetch the remote state, verify that the version is unused, and record the exact
commit to be rehearsed:

```bash
git fetch origin main --tags
git show origin/main:CHANGELOG.md | grep -F '## [0.8.0]'
git rev-parse origin/main
git rev-parse -q --verify refs/tags/v0.8.0  # must print nothing and fail
gh release view v0.8.0 --repo akz142857/Halro  # must report not found
```

Do not continue until the `main` CI for that commit is green. Avoid merging
anything else to `main` between the dry run and the formal release.

### 2. Verify the one-time package automation setup

The same least-privilege GitHub App must be installed on all four repositories:

- `akz142857/Halro`;
- `halro-ai/homebrew-tap`;
- `halro-ai/apt-repository`;
- `akz142857/Halro-website`.

It needs repository Contents, Issues, and Pull requests read/write. Each
repository must expose `HALRO_RELEASE_APP_CLIENT_ID` as an Actions variable and
`HALRO_RELEASE_APP_PRIVATE_KEY` as an Actions secret. Listing secrets shows only
their names, never their values:

```bash
for repository in \
  akz142857/Halro \
  halro-ai/homebrew-tap \
  halro-ai/apt-repository \
  akz142857/Halro-website
do
  printf '\n%s\n' "${repository}"
  gh variable list --repo "${repository}"
  gh secret list --repo "${repository}"
done
```

In `halro-ai/apt-repository`, verify that the protected `apt-production`
Environment exists. It must contain the dedicated online archive-signing key as
the Environment secret `HALRO_APT_ARCHIVE_SIGNING_KEY` and its uppercase full
fingerprint as the Environment variable
`HALRO_APT_ARCHIVE_SIGNING_FINGERPRINT`. Never print or copy the private key
through a workflow log.

```bash
gh api repos/halro-ai/apt-repository/environments \
  --jq '.environments[].name'
gh secret list --repo halro-ai/apt-repository --env apt-production
gh variable list --repo halro-ai/apt-repository --env apt-production
```

Also verify that the production cluster's `ghcr-credentials` pull secret can
read the private `ghcr.io/halro-ai/apt-repository` snapshot image. A dry run does
not execute `publish`, `container-push`, or the downstream repository jobs, so a
green dry run does **not** prove that any of these credentials are configured.

### 3. Run the non-publishing rehearsal

Start the only supported release workflow on `main` with `dry_run=true`:

```bash
gh workflow run release.yml \
  --repo akz142857/Halro \
  --ref main \
  -f version=v0.8.0 \
  -f dry_run=true
```

Find and follow the new run:

```bash
gh run list \
  --repo akz142857/Halro \
  --workflow release.yml \
  --event workflow_dispatch \
  --limit 5
gh run watch RUN_ID --repo akz142857/Halro
gh run view RUN_ID \
  --repo akz142857/Halro \
  --json headSha,conclusion,url
```

The rehearsal must finish green. Confirm that its `headSha` equals the recorded
`origin/main` commit and that no tag or GitHub Release was created. It builds
and verifies all release artifacts, Debian packages, SBOMs, attestations, and
Sigstore bundles, but publishes none of them.

### 4. Trigger the formal release once

After the dry run is green, recheck that `main` still resolves to the rehearsed
commit. Then make the single formal trigger:

```bash
gh workflow run release.yml \
  --repo akz142857/Halro \
  --ref main \
  -f version=v0.8.0 \
  -f dry_run=false
```

This run repeats the release gates, creates the annotated `v0.8.0` tag only
after they pass, publishes the immutable GitHub Release and GHCR images, and
dispatches the exact version and full commit to Homebrew and APT. A protected
`apt-production` Environment may pause for its configured approval; that is an
approval inside the same release chain, not a second release trigger.

If a transient failure occurs after the tag is created but before the GitHub
Release exists, rerun the failed jobs. A fresh formal dispatch is also accepted
while `main` still points at that same tagged commit: `prepare` verifies that
the existing tag resolves to the exact run SHA and that no Release exists, and
the publish step reuses it. A tag at any other commit, or an already-published
version, remains a hard failure.

### 5. Monitor the four-repository chain

Follow the control-plane workflows in order:

```bash
gh run list --repo akz142857/Halro \
  --workflow release.yml --limit 5
gh run list --repo halro-ai/homebrew-tap \
  --workflow update.yml --limit 5
gh run list --repo halro-ai/apt-repository \
  --workflow publish.yml --limit 5
gh run list --repo akz142857/Halro-website \
  --workflow package-release.yml --limit 5
```

The expected sequence is:

1. Halro publishes the immutable Release and multi-architecture images.
2. Homebrew verifies the released archives, opens `package/v0.8.0`, runs macOS
   and Linuxbrew Formula CI, and auto-merges the protected pull request.
3. APT verifies the released Debian assets, signs and publishes an immutable
   repository snapshot, and asks Halro-website to deploy its exact image digest.
4. Halro-website auto-merges the protected deployment pull request.
5. APT waits for that snapshot, runs clean-host Debian and Ubuntu acceptance on
   amd64 and arm64, and waits for the matching Homebrew Formula.
6. Only after every acceptance cell passes does Halro-website advertise the new
   version and package channels.

An `auto-merge-package-release` run with a grey slashed-circle icon and
`conclusion=skipped` is normal for `main` pushes and unrelated pull requests.
Only automation-owned release branches are eligible to enter the merge job.

### 6. Confirm publication and acceptance

Do not call the release complete merely because the GitHub tag exists. Confirm
each independent channel:

```bash
gh release view v0.8.0 --repo akz142857/Halro
curl --fail --silent --show-error \
  https://packages.halro.ai/apt/repository-version.json
curl --fail --silent --show-error \
  https://raw.githubusercontent.com/halro-ai/homebrew-tap/main/Formula/halro.rb \
  | grep -F '/releases/download/v0.8.0/'
```

Verify that the APT clean-host matrix and both Homebrew Formula jobs passed,
that `brew install halro-ai/tap/halro` and the documented APT installation
produce `v0.8.0`, and that neither installer initializes configuration or starts
a service. Finally confirm that `halro.ai` advertises only channels whose install
acceptance passed.

### 7. Recover without rewriting a published version

- If the run fails before the tag exists, fix the candidate and repeat the
  exact-commit dry run before starting a new formal run.
- If the tag and GitHub Release exist, do not start another formal `v0.8.0`
  release, delete the tag, or replace an asset. After repairing credentials or
  an external dependency, rerun only failed jobs from the original run:

  ```bash
  gh run rerun RUN_ID --failed --repo akz142857/Halro
  ```

- A failed downstream channel can also be retriggered with the exact published
  version and full release commit:

  ```bash
  gh workflow run update.yml \
    --repo halro-ai/homebrew-tap \
    --ref main \
    -f version=v0.8.0 \
    -f commit=FULL_RELEASE_COMMIT
  gh workflow run publish.yml \
    --repo halro-ai/apt-repository \
    --ref main \
    -f version=v0.8.0 \
    -f commit=FULL_RELEASE_COMMIT
  ```

- Keep a failed channel unavailable until its own clean-host acceptance passes.
  A downstream failure never authorizes rewriting the Git tag, GitHub Release,
  checksums, or package assets.

**[1.0.0 target — not in `release.yml` today.]** Configure the GitHub `v1-release` environment with required reviewers. Its
approval is the explicit boundary where reviewers verify the exact-commit GA
Provider matrix, 24-hour soak artifacts, RC checklist, and release description.
Enable Prevent self-review and keep `M11_RELEASE_EVIDENCE_JSON` exclusively as
an Environment secret. A `release-governance` preflight verifies those reviewer
protections before the publish job is scheduled, so a missing environment is a
hard failure rather than an invitation for GitHub to auto-create an unprotected
one. The complete setup and run-archive procedure is
`docs/verification/release-run-evidence.md`.
Asset generation cannot bypass test, Race, Vet, vulnerability, SDK compatibility,
web, SSE stress, container, checksum, SBOM, or signature jobs; publication is a
separate environment-gated job.
The publish job additionally requires an annotated tag whose GitHub verification
object reports a valid GPG, SSH, or S/MIME signature; a lightweight or
unverified tag cannot publish assets. RC tags are marked prerelease, while the
reviewed `docs/milestones/release-notes-v1.0.0.md` is used only for the final tag.

## Where the M11 evidence bundle comes from **[1.0.0 target]**

`publish` verifies `M11_RELEASE_EVIDENCE_JSON` against the artifacts the same run
produced: `tools/m11/release-evidence/verify.py` requires a SHA-256 for each of
the nine release files and recomputes every one of them from the downloaded
`release-assets`. Two of those nine are the container archives, which are not
byte-reproducible (see below), so their digests cannot be computed from the
source tree ahead of time. **The bundle can only be completed after a release run
has built the artifacts it describes.**

The Environment approval pause is that window. `publish` declares
`environment: v1-release`, so once `provenance` has uploaded `release-assets` and
`release-governance` has confirmed the Environment's protections, the job is
scheduled and then waits for a reviewer. The order is therefore:

1. create `v1-release` with required reviewers and Prevent self-review — before
   the tag, because `release-governance` fails the run without it. The evidence
   secret does not have to exist yet;
2. push the signed annotated tag and let the run build, sign, attest, and upload
   `release-assets`;
3. while `publish` waits for approval, download that run's `release-assets`,
   complete the bundle from `tools/m11/release-evidence/template.json` inside the
   restricted evidence system using those exact digests, and install it as the
   `v1-release` Environment secret;
4. have an independent reviewer approve. `publish` then verifies the bundle
   against the artifacts it was written from.

If approval happens before the secret is installed, `publish` fails on the empty
secret. Recover with `gh run rerun --failed`, which reuses the same
`release-assets` — but only inside the 90-day artifact retention window. Once
that window closes, or if anything forces a fresh build, the container digests
change and the bundle has to be rewritten against the new run.

Note the consequence for release candidates: `publish` applies this verification
to every `v*` tag, and the bundle it verifies covers the full M11 production
evidence — the 14 real-AWS KMS scenarios, the recovery drill, and four-role
sign-off. An RC cannot publish on supply-chain evidence alone.

This sequence was derived from a workflow definition that no longer contains it.
`publish` declares no `environment:`, and neither `release-governance` nor
`tools/m11/release-evidence/verify.py` is referenced anywhere in
`.github/workflows/`. The tools still exist and their unit tests run in CI; the
workflow does not call them. Restoring this sequence means adding those steps
back, not just creating the environment.

Before creating an RC tag **[1.0.0 target — the v0.x line has no release candidates]**:

1. run all CI, race, fuzz, SDK compatibility, recovery, and benchmark gates;
2. build from a clean tree and verify embedded UI has no diff;
3. review dependency/license and security reports;
4. run and archive the GA real-account Provider matrix described in
   `docs/verification/provider-real-matrix.md`;
5. run and archive the 24-hour soak on the exact commit as documented in
   `docs/verification/soak-testing.md`;
6. create and push a signed annotated tag;
7. verify every downloaded blob, including the container tarball, against
   `checksums.txt`, its Sigstore bundle, and its GitHub build-provenance
   attestation;
8. run `halro version`, `config check`, backup verify/restore, and a Gateway smoke test on each supported architecture.

The reviewed release description is `docs/milestones/release-notes-v1.0.0.md`. Keep its
status and measured-limit section synchronized with the exact tagged commit;
do not remove unresolved RC, Provider-matrix, or soak conditions before they
have archived evidence.

## Reproducibility scope

The four `halro-<os>-<arch>.tar.gz` archives are byte-reproducible from the tag:
the embedded build date comes from the tag commit's committer date
(`SOURCE_DATE_EPOCH`, shared by every matrix leg and the container build), the
Go build uses `-trimpath`, and packaging pins archive metadata
(`tar --sort=name --owner=0 --group=0 --numeric-owner --mtime` piped through
`gzip -n`). Rebuilding those four on a different machine reproduces the lines in
`checksums.txt` that name them.

`halro-container.tar.gz` and `halro-deadman-container.tar.gz` are **not** byte-
reproducible yet, and `checksums.txt` covers them alongside the archives, so a
line-by-line comparison of the whole file will differ even when every binary is
identical. Two causes, both known:

- `SOURCE_DATE_EPOCH` reaches the image config but not the layer file
  timestamps. Fixing that needs `docker buildx build --output
  type=docker,rewrite-timestamp=true` (BuildKit 0.16+) rather than `docker
  build`, which is a release-pipeline change that must be exercised on a real
  RC run before it is trusted.
- The main `Dockerfile` still uses floating base tags (`node:22-bookworm-slim`,
  `golang:1.26.6-bookworm`, `distroless/static-debian12:nonroot`) while
  `deploy/observability/external-probe/Dockerfile` is digest-pinned. Byte
  identity requires the same base digests.

Measured on 2026-08-12: two `--no-cache` builds of the same commit with the same
`SOURCE_DATE_EPOCH` produced different image IDs and different `docker save |
gzip -n` digests while the `halro` binary inside both was byte-identical
(`1acb6129…`). Verification of the fix is deferred to the RC that exercises it.

RC failures create a new RC tag; published assets are never overwritten.
Preserve every RC release workflow before the 90-day artifact window expires:
`scripts/archive-release-run.sh` downloads the run metadata, complete logs,
release assets, and signed run-evidence manifest and verifies their binding.
It discovers the exact formal or `-dry-run` evidence artifact before creating
the requested output directory, and publishes the local archive atomically so
a failed download can be retried at the same path.
