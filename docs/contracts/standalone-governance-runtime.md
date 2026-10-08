# Default Standalone and Primary Governance owner

Standalone and Primary now select a fresh disk Governance projection and one
`NewProjectedManager` for Report, durable idempotency, current revisions,
paginated history, maturity reads and export. It retains no historical State or
decoded checkpoint. Replica keeps its opaque authenticated native journal; it
does not expose a business Governance Manager.

## Trust and startup

The native Governance journal remains the authority. Before reconstruction,
startup reads Metadata's authenticated external journal anchor. A nonempty
journal requires that anchor; authentication, sequence/offset bounds and the
actual matching frame offset/hash must all hold. This preserves detection of a
deleted complete suffix, which per-frame HMAC alone cannot detect.

With the data-directory lock held and no published writers, startup discards
only the reserved `dataDir/governance-projection.db` and rebuilds an exclusive
fresh file from the complete authenticated journal. Native and derived heads
must match. The final native head/anchor is persisted before publication. Old
Governance checkpoint payload is not loaded; saved derived DB bytes are never a
trusted restart cache. Construction and native Open still scan full history.

For Primary, the authenticated member role, term, promise and configuration
identity must agree before construction and remain unchanged through completion.
The physical projection belongs to Runtime until accepted operations join.
Primary stays nonready until its existing startup continuation confirms the
complete four-store ordering prefix and finishes accounting recovery. Report
uses the real ordering coordinator and the subsequent Metadata anchor write
waits for confirmation of the prefix, including the Governance frame.

The Standalone degradation contract is preserved: a corrupt Governance journal
makes Governance unavailable; it does not publish a usable empty projection.
Independent ordinary accounting can still admit work. Primary instead fails
Open if its Governance projection cannot be constructed; it publishes neither
a degraded member nor an empty replacement. Authority alias failures
are refused before startup mutations instead of proceeding to reconstruction.

## Reserved path and cancellation

Both Standalone Ledger and Governance reserved paths are checked against
configured Metadata/credential/TLS/KMS files, computed native Ledger/Governance/
Audit/Metadata/member/log paths, and both data-directory and sibling publication
lock files. Existing identities are compared, not just lexical names. In
particular, a native symlink pointing into a reserved target must not permit
deleting that target.

Existing reserved files also require a complete identity check over the fixed
authority directories, including computed Metadata journal and sealed Ledger
generation names. Directory names are read 64 at a time; no historical manifest
list or recursive tree is materialized. Successful total IO still grows with
those directories. The scan checks cancellation at entry, each page/entry,
before Stat and at final return. Partial scans cannot authorize deletion;
Ledger/Governance check context again immediately before discard. OS syscalls
remain cooperative and have no hard duration bound.

## Lifetime

Runtime holds the physical DB separately from Manager aliases. Failure cleanup
retires the Manager, joins actual DB readers, closes the DB and native journal,
then releases the directory lock. Normal terminal Close drains accepted Manager
and export operations before closing the physical owner. An alias replaced in a
test or retired Manager does not lose physical ownership. The reserved path is
created exclusively by the storage engine itself; its identity is captured
from the actual opened descriptor. The reserved path is removed only if its
current identity matches that descriptor; replacement files
survive with an explicit error.

## Remaining work

Native Governance now retains 4096 fixed identity/content fingerprints and
native positions, with no Event payload/string keys. Its calibrated
`governance_log_ram_bytes` envelope uses one shared Recovery lifetime claim
until actual native Close and cache retirement; replay and batch/codec
temporaries still require separate fees. Default Ledger/Usage also select disk,
but shared physical fees and full bounded acceptance remain open. Authenticated
HA incremental generation publication, D-03 authenticated incremental roots,
startup space and maintenance-window admission, RAM/disk/mmap/freelist/dirty-page
fees, migration/backup fault matrices and target Linux/production acceptance
remain open. This transition reduces historical State residency; it does not
establish the complete memory/capacity objective.

## Primary local evidence

[Primary Governance integration evidence](../verification/performance/2026-10-04-primary-governance-runtime/README.md)
covers real TLS confirmation, historical rebuild, revisions, durable
idempotency, no-peer reopen and failed-constructor physical cleanup.

## Backup and staged restore

Offline backup and staged restore validate Governance in a fresh temporary disk
projection, including full native authentication, the independent external
anchor, Outcome identity and revision/supersedes continuity. They do not load
historical State or old Governance checkpoint payloads. The temporary DB is
discarded before backup publication or restore validation succeeds. Standalone
backup advances the external anchor only after complete validation and scratch
retirement; Replica validation keeps its Metadata anchor unchanged.

Old checkpoint bytes remain opaque in the Metadata snapshot. New backups name
no selected saved Usage checkpoint (`checkpoint_watermark` is zero), while
native Ledger and independent disk Usage archive checks still run. This keeps
legacy bytes available without materializing an Aggregate or granting a saved
derivative restart authority. Complete bounded backup still requires migration
of the backend's historical file list and unfinished archive obligations.

[Backup validation evidence](../verification/performance/2026-10-04-backup-governance-disk/README.md)
includes real round trips, cancellation, anchor/domain refusal and an encrypted
archive with a valid native chain but invalid revision refused before publish.

[Native cache ownership and retained-heap evidence](../verification/performance/2026-10-04-governance-native-cache/README.md)
records the finite retry window, shared claim, real HA and local growth samples.
