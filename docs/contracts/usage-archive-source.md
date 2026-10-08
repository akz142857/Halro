# Bounded Usage archive source and independent native anchor

Status: local implementation/verification, 2026-10-03. I-07/I-08 ongoing.
Default Runtime still selects Aggregate. The selected disk Runtime export branch
now uses this producer with the finite publisher in [usage-disk-export.md](usage-disk-export.md);
legacy default Export/CLI and retention have not migrated. No saved disk projection is trusted by reopen.

## Native deletion protection

The Standalone/Primary maintenance tick advances the external Ledger chain
anchor independently of Collector CatchUp or old Usage checkpoint success.
Native durability and a derived analytics position are different facts. A
retired/broken Collector cannot leave later native frames outside this anchor.
The unchanged Store transaction enforces monotonic sequence and same-sequence
roll/hash constraints. Native/Metadata IO errors remain explicit logged failures;
this change does not manufacture a durable anchor when the store cannot write.

A selected disk Usage owner catches up through its selected Collector and skips
legacy checkpoint encoding/rollup merging. Its own committed derived DB is not
an authenticated restart snapshot or permission for maintenance compaction.
Replica checkpoint calls remain node-derived only; disk mode skips old checkpoint
writes. All native-anchor entry points explicitly refuse Replica role, since
its native durable tail can contain a suffix outside its applied prefix.
This role guard is not promotion, applied-prefix publication or D-03 trust.

## Complete, frozen source interval

`DiskProjection.StreamArchive` freezes the fresh owner's native head H and
traverses every attempt in `(after_sequence, H.Sequence]` in native sequence
order. Completion dates cannot reorder the source frontier. Its complete-source
receipt preserves full H, including trailing non-attempt records, rather than
using the maximum attempt sequence/date as the completed Ledger prefix.

Pages use ordered seeks after the previous sequence, with explicit page rows,
input bytes and encoded JSON output bytes. Whole-job limits also cap pages,
rows, cumulative input/output and duration. Bounds reject before decoding a
source row that cannot fit; a completed page's transaction/admission ends before
sink encoding or publication IO. Later appends can proceed during that callback
and remain excluded by frozen H. The current fresh format retains all attempt
rows; a future retention floor must additionally reject an unavailable lower
bound instead of certifying an already-trimmed interval.

One zero-queue heavy slot is shared with Summary/detail. A whole-job lifetime
lock joins physical callbacks through cancellation or Close. Callback slices
are borrowed until return; retaining them requires a separate owned copy/fee.
Visitors return only after their owned synchronous IO finishes and must not
call this owner's Close or another heavy query. Ordinary Apply/point reads may
proceed between pages. No DB reader or mutation lock spans a visitor callback.

Bad lower bound, corrupted row/state, exhausted budget, callback failure,
deadline/cancellation or retirement yields a zero receipt plus error. Earlier
callback side effects are incomplete output, so the publisher must preserve or
clean them as uncommitted artifacts and cannot advance manifest H. Even a
successful receipt is in-process source completion, not content authentication,
archive publication, restart checkpoint or authorization to prune anything.
A caller's prior lower cut must belong to the same authenticated native history;
the scalar after value alone does not prove that relationship.

## Remaining publisher and retention work

The producer is a prerequisite, not a bounded replacement for Export. The
publisher must limit rows/input/output and actual Parquet encoder work, stream
checksums, synchronize and atomically publish every partition, and advance H
only after the entire frozen interval succeeds. Date changes inside one source
page cannot authorize an intermediate global watermark. Retry must validate
orphan content before reuse and repeat missing durability barriers.

The manifest directory must be paged/hierarchical with a versioned migration
from schemas 3–8, not a resident `Manifest.Files` slice. Source/native identity,
role-allowed generation, complete-prefix/coverage proof, existing archive
compatibility, retention pins and failure cleanup must be joined into Runtime
and CLI before default disk selection. This does not establish shared physical
RAM/disk fees, Parquet peak allocation, Linux capacity or production acceptance.

## Finite step scheduling follow-up

The shared scanner now also supports [finite complete-prefix archive steps](usage-archive-steps.md).
DiskArchiveCut separates the actual frozen physical Head from ThroughSequence,
the explicit successfully covered interval. StreamArchive keeps its strict whole-
source contract (ThroughSequence == Head.Sequence); step mode can stop at a lower
complete native sequence prefix on a job budget boundary. Selected Runtime
maintenance uses that step publisher, with all source/sink/lifetime errors still
refused. The receipt never invents physical generation/offset evidence for an
intermediate sequence and never authorizes native compaction.
