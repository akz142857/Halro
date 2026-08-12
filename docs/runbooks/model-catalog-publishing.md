# Signed model catalog publishing and key rotation

This runbook is the production procedure for the optional dynamic model
catalog. Halro is safe with the feature disabled; do not enable it until every
activation gate below is evidenced for the target repository and release.

## Roles and activation gates

Three responsibilities stay separate:

1. A catalog author prepares reviewed exact model facts and cannot publish.
2. A signer operates an isolated Ed25519 KMS/HSM or offline workstation and
   cannot merge the publication PR.
3. Two independent reviewers approve publication: one protected-environment
   reviewer permits the workflow, and a different CODEOWNER approves the PR.

Before first use, a repository administrator must:

- create the `model-catalog-production` GitHub environment with required
  reviewers and prevent self-review;
- require two approving reviews and CODEOWNER review on `main`, dismiss stale
  approvals, require the `verify-publication` check, require the PR branch to be
  up to date before merge (or use a merge queue that reruns the check), and
  prohibit bypass/force-push;
- set the repository variable `MODEL_CATALOG_TRUST_ROOTS` to public roots in
  `key-id|base64-ed25519-public-key|not-before-RFC3339|not-after-RFC3339`
  format, separated by semicolons;
- set the repository variable `CATALOG_PUBLISHER_LOGIN` to the dedicated bot
  login allowed to author publication PRs;
- for the workflow as committed, store a fine-grained PAT owned by that bot as
  the protected-environment secret `CATALOG_PUBLISHER_TOKEN`, limited to branch
  contents and pull-request creation for this repository; the workflow's
  ordinary `GITHUB_TOKEN` remains read-only so the resulting PR triggers CI;
- if policy requires a GitHub App instead, never store an installation token:
  it expires after one hour. Store the App ID/private key under the protected
  environment, add a reviewed full-SHA-pinned token-generation action, and pass
  its per-run output to both checkout's `token` input and `GH_TOKEN`;
- build and publish Halro artifacts through `release.yml`, which injects the
  same public roots into binaries and containers;
- verify Admin reports a non-zero trust-root count before setting
  `model_catalog.enabled: true`.

Public roots are not secrets. Private keys and signing credentials must never
enter this repository, GitHub Actions secrets, Halro configuration, logs, or
build artifacts.

## Prepare and sign a candidate

Start from the validated
[`catalog/unsigned-snapshot-v1.example.json`](../../catalog/unsigned-snapshot-v1.example.json)
shape and replace every example identity with reviewed exact values. Set schema version and
capability dictionary version to 1, use a new strictly increasing positive
sequence, choose bounded UTC generation/expiry times wholly inside the signing
root's validity interval, and include only exact provider/profile/target-kind/
model/region identities and reviewed capability IDs. Revocation is exact and
must carry no capability claims.

Prepare canonical signing bytes on a trusted review workstation:

```sh
umask 077
go run ./tools/modelcatalog/prepare unsigned-snapshot.json canonical-payload.json
```

Review the canonical payload and printed `sha256:` revision. Send the exact
bytes of `canonical-payload.json` to the isolated Ed25519 KMS/HSM or offline
signer. Transfer back only the base64-encoded 64-byte signature and public key
ID. Assemble the envelope without exposing a private key to Halro tooling:

```sh
MODEL_CATALOG_TRUST_ROOTS='release-2026|PUBLIC_KEY|NOT_BEFORE|NOT_AFTER' \
  go run ./tools/modelcatalog/assemble \
  canonical-payload.json release-2026 signature.base64 \
  catalog/candidates/model-catalog-v1.json
MODEL_CATALOG_TRUST_ROOTS='release-2026|PUBLIC_KEY|NOT_BEFORE|NOT_AFTER' \
  go run ./tools/modelcatalog/verify catalog/candidates/model-catalog-v1.json
```

The signer must independently compare the revision, sequence, expiry, entry
count, and change request. Never sign bytes reconstructed by an unreviewed
service. Keep the candidate expiry short enough to limit stale facts and long
enough for the supported offline window.

## Publish

1. Push the signed candidate to `catalog/candidates/<name>.json` on a dedicated
   commit. Record its full 40-character commit SHA; mutable branch names are not
   accepted. Do not merge that candidate path into `main`.
2. Dispatch `publish signed model catalog` from trusted `main`, supplying the
   immutable candidate commit SHA and candidate path.
3. The protected-environment reviewer checks the external change ticket,
   canonical revision, signer/key ID, sequence, expiry, and diff before
   approving the job.
4. The workflow verifies signature/schema/limits, rejects non-increasing
   sequence, copies the candidate to `catalog/model-catalog-v1.json`, and opens
   a publication PR under the configured publisher identity.
5. A different CODEOWNER reviews the exact artifact, candidate SHA/revision/
   sequence recorded in the PR, and the `verify-publication` required check.
   That check rejects any other PR author/repository/branch, a stale base SHA,
   extra changed paths, or a sequence not strictly above current `main`. Merge
   only through protected and up-to-date `main` (or its merge queue).
6. After publication, manually refresh one canary instance. Confirm `current`,
   expected revision/sequence, success audit, and metrics before wider refresh.

Never edit or directly push `catalog/model-catalog-v1.json`. Never republish a
sequence with different content. To undo a bad fact, publish a new higher
sequence that narrows or exactly revokes it; do not roll back the file.

## Key rotation

1. Create the new private key inside the isolated signer and export only its
   public key.
2. Add the new public root while retaining the old root, then release Halro
   binaries and containers that trust both roots.
3. Verify the overlap release is deployed wherever dynamic updates are enabled.
4. Publish a catalog signed by the new key. During overlap, envelopes may carry
   both signatures to support staggered readers.
5. Retain the old root until every supported old Halro release is retired and
   the longest catalog signed by the old key has expired.
6. Remove the old root in a later release and record the retirement evidence.

If a private key may be compromised, stop publication, disable updates or pin
the last approved revision, publish a release that removes the compromised
root, and only then resume with a new root. TLS or GitHub history does not
replace signature trust.

## Failure and recovery

- Download, redirect, DNS/IP, size, compression, schema, signature, expiry,
  pin, and rollback failures must leave the active last-known-good unchanged.
- If no verified last-known-good exists, Halro uses the bundled snapshot and
  reports `catalog_unavailable`; existing immutable Deployment snapshots keep
  serving.
- An expired verified last-known-good is visible as degraded and retained for
  provenance/rollback protection, but new resolution uses bundled facts only;
  it is never presented as current.
- Use Admin status, `halro_signed_model_catalog_*` metrics, and
  `model_catalog.*` audit events. Do not log or attach raw catalog bodies to
  incidents.
- Recovery always publishes a newly signed, higher sequence or a new Halro
  release. Deleting local state or lowering sequence is not a recovery method.
