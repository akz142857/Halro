# Release Process

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

Configure the GitHub `v1-release` environment with required reviewers. Its
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

Before creating an RC tag:

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
  `golang:1.26.5-bookworm`, `distroless/static-debian12:nonroot`) while
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
