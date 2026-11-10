# Release run evidence and archival procedure

Status: current for the v0.x release workflow. This document describes the
controls that `.github/workflows/release.yml` enforces today and does not claim
that a release run, publication, or independent approval has occurred.

The v0.x line has no CI-enforced release-candidate or reviewer-approval gate.
The owner makes the go/no-go decision using
`docs/verification/release-assessment.md`. The protected `v1-release`
Environment, `release-governance` preflight, signed-tag requirement, and
`M11_RELEASE_EVIDENCE_JSON` verification described in older 1.0.0 plans are not
part of the current workflow. Creating an Environment or secret does not make
them active; restoring those controls requires a reviewed workflow change.

## 1. What the current workflow enforces

Only a manual `workflow_dispatch` on the default branch enters `prepare`;
pushing a tag does not start this workflow. Dispatch requires a successful
ordinary push CI run for the exact main commit. It may be a non-publishing dry
run; a publishing dispatch creates its annotated tag only after the preceding
jobs pass. `prepare` validates the version syntax and requires a matching
CHANGELOG section and a completed assessment record.

The dependency graph then enforces these jobs and steps:

- `website-docs`: immutable website candidate checkout, exact release identity
  and API contract verification, bilingual website build and release-content
  checks before `provenance` can run;
- `quality`: repository/license boundary, `go test ./...`,
  `go test -race ./...`, `go vet ./...`, and `govulncheck`;
- `sdk-compatibility`: the Python, Node, and Go official-SDK black-box
  contracts;
- `stress`: the 1,000-connection SSE cleanup test;
- `web`: frontend tests, `npm audit --audit-level=moderate`, and the production
  build uploaded as `admin-dist`;
- `binaries` and `container`: licensed static archives for the supported
  OS/architecture matrix, non-root container archives, and Trivy scanning;
- `provenance`: source and released-binary SBOMs, build attestations,
  checksums, Sigstore bundles, and the signed run-evidence manifest;
- `publish`: checksum, attestation, and Sigstore verification before tag and
  GitHub Release creation; and
- `container-push`: architecture images and multi-architecture version and
  `latest` manifests after publication;
- `website-acceptance`: after container publication, wait for the public bilingual
  docs, installation pins and API source revisions to match the release. See
  [the docs companion procedure](website-docs-release.md) for deployment order
  and bounded recovery without replacing release assets.

There is no `environment:` on `publish`, no independent approval pause, no
`release-governance` job, and no M11 evidence-secret check. The release graph
does not run the fuzz suites. It does rebuild the web bundle and compare
`internal/webui/dist` with the committed artifact before release; that drift
check is a current release-workflow gate.

## 2. Evidence emitted by every completed provenance job

`provenance` uploads two 90-day workflow artifacts:

- `release-assets`, containing the archives, both SBOMs, checksums, and their
  Sigstore bundles; and
- `release-run-evidence-RUN_ID-ATTEMPT` (with `-dry-run` on a rehearsal),
  containing a signed `release-run-evidence.json`.

The manifest binds the workflow run ID and attempt, exact commit, intended tag
ref, workflow identity, and the name, size, and SHA-256 digest of every file in
`release-assets`. It is generated after the release-file signatures exist, so
it covers those bundles without placing itself in the release directory and
creating a self-referential digest.

The docs jobs separately upload `website-docs-candidate-RUN_ID-ATTEMPT` and
`website-docs-public-RUN_ID-ATTEMPT` with 90-day retention. They bind the intended
release to the prepared website SHA/manifest digest and the checked live paths,
respectively. These JSON records are workflow evidence, not signed release
assets; the public result necessarily occurs after publication.

For a publishing run, `publish` starts as soon as its declared dependencies
are satisfied. Archive operators must not wait for an approval pause that the
workflow does not contain.

## 3. Preserve a run before its retention window expires

Install and authenticate `gh`, and install `jq`, `cosign`, `python3`, and
`shasum` on the archival host. Do not delete the run. Download its complete
logs and both artifact sets into restricted, immutable evidence storage:

```sh
./scripts/archive-release-run.sh RUN_ID \
  /secure/release-evidence/VERSION-RUN_ID
```

The script is read-only against GitHub. It refuses to overwrite an existing
archive, verifies that run ID, attempt, and commit match GitHub metadata,
verifies the manifest's Sigstore identity, recomputes every recorded artifact
digest, saves the complete workflow log, and writes `archive-sha256.txt` over
the resulting archive.

The archive script covers the signed provenance artifacts, not the separate
website acceptance records. Download those with `gh run download RUN_ID --name
ARTIFACT_NAME --dir DESTINATION` into a separate restricted evidence directory
and record its hashes and final website deployment run. When only acceptance is
rerun, retain the successful candidate artifact from its earlier attempt as well
as the new public artifact. Do not modify an already-hashed release archive.

Record the secure-storage locator, run URL, run ID, attempt, commit, tag, and
`archive-sha256.txt` digest in the release assessment record. Do not commit
Provider credentials or evidence, M11 material retained for a future 1.0.0
process, full run logs, or the restricted archive to this public repository.

## Acceptance boundary

Repository tests prove the manifest and archival tooling behavior. A specific
release has retained run evidence only after its live GitHub run and downloaded
archive have passed this procedure. That evidence does not retroactively add
the retired 1.0.0 approval gates to a v0.x release.
