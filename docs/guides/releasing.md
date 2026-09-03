# Release Process

> **Status.** Two things are described here, and until now the document did not
> separate them. The **v0.x line** is what `.github/workflows/release.yml`
> actually does today: nine jobs — `prepare`, `quality`, `sdk-compatibility`,
> `stress`, `web`, `binaries`, `container`, `provenance`, `publish` — with the
> only publication precondition being `prepare`'s CHANGELOG section check and a
> `v*` tag. There is no environment approval, no `release-governance` preflight,
> no signed-tag requirement, and no M11 evidence verification in that workflow.
>
> The **1.0.0 governance gates** — the `v1-release` environment with required
> reviewers, the `release-governance` preflight, the annotated-signature
> requirement, and `M11_RELEASE_EVIDENCE_JSON` verification — are a target, not
> the current pipeline. They were retired for the v0.x line by owner decision on
> 2026-08-16, recorded in `docs/verification/release-assessment.md`: the v0.x
> line has no release candidates and no CI-enforced release gates, and the
> go/no-go is the owner's, made against a filled assessment record.
>
> Sections describing the 1.0.0 gates are marked **[1.0.0 target]**. Do not
> follow them when tagging a v0.x release: the approval they tell you to wait
> for will not appear, and the evidence bundle they tell you to prepare is not
> read by anything.

## What the v0.x pipeline does

Pushing a `v*` tag runs the full matrix and publishes if `prepare` passes. The
gates that genuinely hold are the ones inside those jobs: `go test`,
`go test -race`, `go vet`, `govulncheck`, the fuzz targets, the official-SDK
compatibility suite, the SSE stress run, the frontend suite and bundle-drift
check, Trivy on the container, reproducible packaging, dual SBOMs, cosign
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
- a non-root `halro-deadman-container.tar.gz` image archive built from
  digest-pinned base images;

- four version-stamped, stripped binary archives, each containing `LICENSE`,
  `NOTICE`, `THIRD_PARTY_NOTICES.md`, and `README.md`;
- a non-root distroless container image exported as `halro-container.tar.gz`;
- an SPDX JSON source/dependency SBOM and a separate SPDX JSON SBOM generated
  from the released binaries;
- SHA-256 checksums;
- a Sigstore keyless bundle for each binary archive, the SBOM, and checksum file;
- a GitHub build-provenance attestation for every archive and SBOM, verified
  with `gh attestation verify` before publication;
- workflow artifacts; and, for a signed `v*` tag, an immutable GitHub Release.

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
