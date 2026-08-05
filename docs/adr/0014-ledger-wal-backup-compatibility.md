# ADR 0014: Ledger WAL evolution and backup/restore compatibility

- Status: Accepted
- Date: 2026-08-04
- Tracking: GitHub Issue #76
- PRD: `docs/prd/prd-versioned-model-pricing.zh-CN.md`

## Context

The existing Ledger uses `HLDG` frame version 1, a fixed header, CRC-protected
JSON payloads, and event kinds 1 through 5. An old reader treats any other
frame version or event kind as corruption. Versioned pricing adds new payload
semantics and event kinds; allowing an old binary to call them corrupt could
trigger unsafe repair or recovery decisions.

Backups contain bbolt metadata, a precise Ledger prefix, Audit, Usage
checkpoint, and committed Parquet files. Pricing adds cross-references and
clock high-water state that must form a recoverable, explainable snapshot.

## Decision

### WAL format evolution

Existing WAL bytes are never rewritten. New pricing/accounting records use
`HLDG` frame version 2; the 24-byte header layout and CRC coverage remain the
same. The frame-version byte is also the payload feature epoch:

- the new reader accepts v1 and v2 frames in the same WAL;
- v1 events decode with `legacy_unversioned` price evidence and retain their
  persisted tokens and committed cost exactly;
- v2 requires the tagged lease/snapshot schema and recognizes the new
  adjustment event kind;
- an unsupported frame or payload epoch returns stable
  `ErrUnsupportedVersion`, distinct from `ErrCorrupt` and from an incomplete
  tail;
- unknown fields within a supported JSON payload may be ignored only when the
  supported epoch defines them as optional; unknown event kinds fail closed as
  unsupported, not corrupt.

Before the first v2 append, bbolt schema migration durably records the minimum
Ledger reader version and enabled feature epoch. Old binaries reject the newer
bbolt schema before opening the WAL for mutation. The new reader may append v2
only after verifying that gate. A crash after the metadata gate but before the
first v2 frame is safe and retryable.

Frame CRC, sequence, event ID, and all v1 bytes remain unchanged. There is no
claim of a cross-record Ledger hash chain; Audit chain integrity remains a
separate security property.

### Backup consistency boundary

The existing offline exclusive data-directory lock remains the backup barrier.
Online backup is not introduced by this milestone. Runtime pricing mutations,
lease creation, and cancellation cannot run while backup owns the lock.

The backup manifest format advances and records at least:

- minimum compatible binary/reader version and Ledger feature epoch;
- bbolt schema version, metadata transaction ID, and pricing high-water state
  digest;
- exact Ledger generation/offset/sequence and last observed frame epoch;
- Audit summary/checkpoint, Usage checkpoint schema/watermark, and both Parquet
  manifest versions/watermarks;
- pricing quarantine state and a digest of unresolved pin/Audit intents;
- Key Slot descriptor digest and existing cryptographic metadata.

Within the barrier, backup verifies pending intents, snapshots bbolt, copies an
fsynced complete Ledger prefix, validates Audit and Usage, pins Settlement and
Adjustment Parquet manifests, and creates the authenticated archive. A Ledger
price-version reference must exist in the metadata snapshot or be explicitly
classified as an allowed orphan whose self-contained `PriceSnapshot` remains
sufficient for calculation. Any other orphan fails backup and restore.

### Restore and quarantine

Restore remains an offline staging operation followed by atomic data-directory
publication. Before publication it verifies format compatibility, Master Key
and Vault, bbolt schema, WAL frame epochs/CRC/sequence, Ledger replay, pending
intent reconciliation, Audit, Usage rebuild, both Parquet datasets, price
selection, and token/cost totals.

Restore enters per-deployment `pricing_quarantine` when a scheduled price is
now effective, post-backup cancellation or successor creation is possible, the
restored pricing high-water conflicts with trusted time, or price/pin/Audit
watermarks disagree. Quarantined deployments fail closed until an authorized
administrator performs recent re-authentication and appends a restore-confirm
Audit event or creates a correct successor version. Restore never silently
reactivates a scheduled version from an old backup.

Rollback after v2 data exists is performed only by restoring the verified
pre-migration backup. Old binaries may not open the upgraded live directory.
Partial bucket downgrade or WAL rewriting is forbidden.

## Orphan-reference rules

- v1 `legacy_unversioned` events need no Price Version reference.
- v2 metered/free Settlements require a self-contained snapshot; a missing
  navigational Price Version is allowed only when the manifest records the
  orphan and the snapshot digest validates.
- pending leases, pin intents, or adjustment events with missing/contradictory
  authority are never allowed orphans and keep readiness closed.
- missing proposal/source navigation may degrade evidence display but cannot
  change a settled amount when the authoritative digest is present.

## Rejected alternatives

### Append new kinds under frame version 1

Rejected because old readers would misclassify a compatible upgrade as
corruption.

### Rewrite the WAL into a new homogeneous file

Rejected because it changes historical bytes, CRCs, sequences, and backup
evidence.

### Add online backup concurrency in this milestone

Rejected because the existing exclusive offline barrier is simpler and already
defines the operational contract. A future online snapshot protocol needs a
separate ADR.

### Restore and immediately trust wall-clock-effective scheduled prices

Rejected because the backup may predate cancellation or successor creation.

## Consequences

- WAL scan APIs expose unsupported-version errors separately from corruption
  and partial-tail repair.
- Migration and backup manifests carry explicit compatibility gates.
- Backup creation may fail on unresolved cross-store intents or invalid
  orphans rather than producing an ambiguous archive.
- Restoring an old but valid backup may require manual pricing review before
  traffic resumes.

## Required verification

- byte-for-byte v1 fixture preservation and mixed v1/v2 replay tests;
- old-reader fixture proving v2 returns `ErrUnsupportedVersion`, not corruption;
- unknown future epoch/kind, partial tail, CRC mutation, and sequence tests;
- kill points before/after schema gate and first v2 append;
- backup tests for valid references, allowed settled orphans, forbidden pending
  orphans, and concurrent mutation exclusion by the data lock;
- isolated restore drill covering metadata, WAL, Audit, checkpoint rebuild,
  both Parquet datasets, price selection, totals, and readiness;
- scheduled-price backup/cancel/time-advance/restore quarantine scenario;
- rollback rehearsal using the exact verified pre-migration archive.
