# Usage archive reader lifetime and reclamation epoch

Local implementation, 2026-10-03. This is a prerequisite for date retention;
the finite persisted root rewrite/unlink protocol and default disk Runtime
selection remain unfinished.

## Admission and actual exit

Each Exporter has four reader slots, with no waiting queue. Public root/catalog
reads and the complete checksum job take one slot and a shared archive lifetime
lock. Internal helpers reuse the caller's permit; paginating or opening each data
file does not consume another slot or release the enclosing job. Legacy
LoadManifest, Verify and Reconcile use the same lifetime boundary, while retaining
their existing historical arrays/maps and IO costs.

The permit remains held through actual descriptor closes and post-close context
checks. Cancellation alone does not release it. Error, cancellation or permanent
retirement withdraws returned positions, pages and reconciliation reports.
Checksum/root readers propagate actual Close errors. Checksums still prove file
content only, without granting canonical row-set, native or deletion authority.

Returned ManifestFile descriptors do not protect arbitrary file IO opened by a
caller after ReadArchiveFiles returns. Such a consumer must obtain an enclosing
owned lifetime before opening the files. The complete checksum job does this.
No public escape hatch grants a caller a deletion permit.

## Reclamation and continuations

The private reclamation gate acquires the Exporter writer mutex and then the
exclusive archive lifetime, both with zero queue. Active readers/writers cause
Busy before the mutation callback runs. The callback must independently prove
its date/native/reference authority and publish its logical root before physical
unlink. Exclusion itself supplies none of those proofs.

An explicit advancement increments a fixed ephemeral uint64 epoch once per
operation, before root publication or deletion. No-op/unsupported operations do
not advance it. Overflow fails before mutation. A failure after advancement
conservatively invalidates continuations; it never rolls back the epoch.

Signed archive cursors initially carried version 1, epoch, immutable page reference,
index and captured archive H. A normal append preserves old immutable pages and
does not advance the epoch. Reclamation rejects an older epoch before opening a
page, including a signed empty/EOF continuation. Versionless tokens are refused.
Exporter keys and epochs remain ephemeral; reopening invalidates old tokens and
does not authenticate saved state.

The subsequent [date candidate format](usage-date-catalog-candidate.md) uses
cursor version 2 to bind retention metadata alongside that captured H/reference.
Older versions fail explicitly; the same epoch/actual-reader lifetime applies.

Legacy PruneBefore uses this gate and advances only when files will be removed.
Its historical list/root rewrite/unlink algorithm remains; paged roots are still
refused. This does not supply the missing finite date-retention protocol.

## Terminal ownership

Close permanently closes admission first, then joins the Exporter writer mutex
and all actual readers before clearing cursor keys and window/publication proofs.
It also actually closes/removes the one idle private date-comparison index;
each [finite comparison step](usage-date-catalog-verification.md) retains its
writer/heavy-query lifetime through physical IO, callback exit and error cleanup.
Writers recheck retirement before publishing successful receipts. Close may wait
for an uninterruptible syscall or callback; logical cancellation is no join proof.

Runtime closes the Exporter after background work and final shutdown checkpoint
work, before accounting/native/Metadata dependencies and the directory lock.
Both Standalone/Primary and Replica close branches use this order. Offline fresh
references and legacy offline resource cleanup also join the Exporter before
closing projections/dependencies or releasing the existing-directory lock.

The exclusion is local to one Exporter. Its owner must retain exclusive data
directory ownership and immutable-file discipline; another Exporter or external
process cannot be governed by this in-memory mutex. The four slots are fixed
local controls, not calibrated shared RAM/disk fees or Linux capacity acceptance.

Evidence: [reader lifetime verification](../verification/performance/2026-10-03-usage-archive-lifetime/README.md).
