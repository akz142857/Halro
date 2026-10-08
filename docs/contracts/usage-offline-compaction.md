# Offline Usage compaction through fresh disk references

Status: local implementation, 2026-10-03. Actual `halro usage compact` now uses
the same fresh disk reference and final authority/cleanup barrier as
[offline verification](usage-offline-verification.md). It no longer constructs
Aggregate, Snapshot, a historical row/date map or a Manifest.Files array.
Default Runtime Usage, prune and rebuild-summary remain resident; I-08 is ongoing.

## Full target through finite steps

```text
halro usage compact --config ./config.yaml
```

The server must be stopped. Offline mutation remains unavailable on configured
HA members or a directory containing member state after configuration removal.
The existing exclusive data-directory lock covers authentication, fresh Ledger
and Usage DB construction, publication, final verification and physical cleanup.
Native authentication, historical Metadata checkpoint binding, independent
Metadata applied/transaction evidence and the final native/Metadata/file-identity
checks use the shared reference owner. Existing derivative DBs/checkpoints do
not certify source history and are never reused or rewritten by this command.

An existing archive is exactly reconciled against genesis through its current
H **before migration or append**. Missing partitions, wrong hashes/rows, duplicate
IDs/paths/sequences, empty nonzero coverage and roots ahead of the native source
fail without changing existing archive files. A missing root starts reconstruction
from genesis; content-addressed orphan matching still applies. The command does
not guess a retention floor. A retained suffix or noncontiguous date-based holes
need the unfinished coverage protocol before this command can accept them.

Within one frozen reference, the command repeatedly exports finite complete
prefixes. Each default step permits 16 source pages, 256 attempt rows and 8 MiB
source input/output; individual source pages, partition encoders, sink work and
actual writes retain their existing hard limits. At most 64 steps are admitted,
within the shared operation's cooperative one-minute context. Receipt source
head remains the actual processed-event watermark; each step's AfterSequence
must equal the preceding committed frontier and ThroughSequence must progress
toward that frozen head. Empty/no-new-attempt history still receives a complete
root and repeats its durability barriers.

Success requires reaching the entire frozen semantic target, then an exact
genesis-to-final-H archive comparison, independent native authority revalidation
and physical reference cleanup. The command returns a bounded
`UsageCompactionReport`, containing `archive` (constant-size ArchivePosition),
`after_sequence`, `through_sequence`, `steps`, `written_rows`, and the explicit
scoped `verification` report. `archive.Files` is a count, replacing the previous
unbounded Manifest.Files list. Archive/source/allowed/native positions remain
distinct, including physical empty-roll boundaries.

Step exhaustion, cancellation, IO/encoding/authentication/verification or cleanup
errors return no success report. Context is rechecked after physical cleanup too:
a cancelled caller waits for
actual reader/DB close and directory-lock release, then receives no success scope.
Already committed complete prefixes and
retryable partition orphans can remain: this operation is not an all-or-nothing
transaction over the entire backlog. A later invocation reconstructs and
authenticates a new reference, exactly validates its current root, then resumes
from that frontier. A limit error names the committed and target sequences.
Whole-history verification itself remains a finite job; history exceeding its
caps cannot bypass it by claiming that a newly published partial prefix is done.

## Preserve startup compatibility until the default owner migrates

`Exporter.ExportDiskStepPreservingCatalog` keeps existing paged catalogs paged.
For a missing or legacy catalog it publishes schema-8 legacy JSON through a
streaming writer. Default Runtime still refuses schema-9 paged roots, so offline
compaction must not make an otherwise supported directory impossible to start.
The normal selected-disk Runtime's `ExportDiskStep` continues to publish paged
catalog v1; the compatibility method does not change that final format target.

Legacy publication reuses the captured root descriptor for a second bounded-value
walk, checking its complete position/digest and file identity. Missing per-entry
schemas inherit the fully validated old root schema **before** the root becomes
schema 8. Existing physical partitions are preserved. New descriptors stream
from the private finite stage file; each old/new entry validates, new entries lie
strictly beyond the previous frontier, and actual entry counts must match.
One entry, a fixed reader buffer and scalar counters are live at a time.

Root output is capped at 1 GiB, with actual root writes sharing the step's write
ceiling (default 256 MiB) and its work ceiling (default 1 GiB). Second-root/stage
reads also charge the work ceiling. Old identity/context checks precede atomic
rename; file Sync and directory barriers preserve publication semantics. An
unchanged complete root is not rewritten but repeats durability checks. Temporary
root/stage cleanup removes only matching owned inodes; cleanup or descriptor-close
failure withdraws the receipt. A failed final directory barrier can leave a
visible, unacknowledged root; retry must validate it and repeat barriers.

This legacy compatibility route retains historical root scan/rewrite IO. It is a
transition toward default paged Runtime startup, not indexed catalog completion.
Shared physical fees, default Runtime migration, retained-date coverage,
resumable whole-history verification, orphan/crash cleanup and D-03 trusted
restart still require work. The command neither deletes native facts nor
authorizes pruning, promotion, production capacity or release acceptance.

Local source-cut/gate evidence: [verification record](../verification/performance/2026-10-03-usage-offline-compaction/README.md).
