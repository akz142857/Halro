# Release Process

The release workflow builds the embedded React UI once and cross-compiles
static Heimdall binaries for Linux and macOS on amd64 and arm64. Windows is not
a v1 target because the exclusive data-directory lock currently uses Unix
`flock` semantics.

Every release run produces:

- four version-stamped, stripped binary archives, each containing `LICENSE`,
  `NOTICE`, `THIRD_PARTY_NOTICES.md`, and `README.md`;
- a non-root distroless container image exported as `heimdall-container.tar.gz`;
- an SPDX JSON SBOM;
- SHA-256 checksums;
- a Sigstore keyless bundle for each binary archive, the SBOM, and checksum file;
- workflow artifacts; and, for a signed `v*` tag, an immutable GitHub Release.

Configure the GitHub `v1-release` environment with required reviewers. Its
approval is the explicit boundary where reviewers verify the exact-commit GA
Provider matrix, 24-hour soak artifacts, RC checklist, and release description.
Asset generation cannot bypass test, Race, Vet, vulnerability, SDK compatibility,
web, SSE stress, container, checksum, SBOM, or signature jobs; publication is a
separate environment-gated job.
The publish job additionally requires an annotated tag whose GitHub verification
object reports a valid GPG, SSH, or S/MIME signature; a lightweight or
unverified tag cannot publish assets. RC tags are marked prerelease, while the
reviewed `docs/release-notes-v1.0.0.md` is used only for the final tag.

Before creating an RC tag:

1. run all CI, race, fuzz, SDK compatibility, recovery, and benchmark gates;
2. build from a clean tree and verify embedded UI has no diff;
3. review dependency/license and security reports;
4. run and archive the GA real-account Provider matrix described in
   `docs/provider-real-matrix.md`;
5. run and archive the 24-hour soak on the exact commit as documented in
   `docs/soak-testing.md`;
6. create and push a signed annotated tag;
7. verify every downloaded blob, including the container tarball, against
   `checksums.txt` and its Sigstore bundle;
8. run `heimdall version`, `config check`, backup verify/restore, and a Gateway smoke test on each supported architecture.

The reviewed release description is `docs/release-notes-v1.0.0.md`. Keep its
status and measured-limit section synchronized with the exact tagged commit;
do not remove unresolved RC, Provider-matrix, or soak conditions before they
have archived evidence.

RC failures create a new RC tag; published assets are never overwritten.
