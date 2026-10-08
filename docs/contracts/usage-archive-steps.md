# Finite complete-prefix Usage archive steps

Local implementation/verification in progress, 2026-10-03. This continues I-08;
default Usage remains Aggregate and native compaction/retention is closed in
selected disk mode. These controls are not D-04/Linux capacity acceptance.

## Receipt and publication

`Exporter.ExportDiskStep` serializes one finite source/encode/publish operation.
Its `DiskArchiveCut.Head` preserves the actual frozen physical source watermark.
`AfterSequence` is the old published root frontier; `ThroughSequence` is the
scalar native sequence through which this step covers every attempt. A lower
complete prefix must never manufacture generation/offset/hash coordinates for
that sequence or publish `Head.Sequence` as if the remaining rows were exported.

The source walks attempts in native sequence order under the frozen Head.
Reaching a whole-job page/row/input/output boundary after at least one row can
finish the lower prefix at its final emitted attempt sequence. It cannot skip a
row that fails to fit a page. When the ordered seek proves no further attempts
through the frozen Head, the receipt advances ThroughSequence to Head.Sequence,
including trailing non-attempt facts. An actual genesis/non-attempt-only source
can therefore complete without fabricating an attempt.

Per-page input/output limits split the page before another row. A row that
cannot fit an empty page, or a whole-job budget that cannot admit the first row,
is a refused job with no report. Corrupt state/rows, visitor/publication IO,
encoder/work/write limits, cancellation/deadline and retirement always refuse the
job; they do not manufacture a smaller successful prefix from an unfinished sink.
The strict `StreamArchive` / `ExportDisk` APIs still require their entire frozen
interval and fail on whole-job exhaustion. The shared exact-reference scanner
also records its explicit interval in ThroughSequence without changing Head.

The publisher stages and durably commits every partition for the complete
finite interval, then appends immutable catalog pages and atomically publishes
only ThroughSequence in the schema-9 root. It checks receipt AfterSequence equals
the prior root and old frontier <= ThroughSequence <= source Head.Sequence.
Every newly staged partition must lie strictly above the old frontier and at or
below ThroughSequence. A scalar prefix does not authorize native trimming.

## Retry and ownership

Each successful root is the restart cursor for the next step, including a new
Exporter instance. No resident backlog queue, global date map or task journal is
needed to resume publishing a native prefix. Matching same-name partition
orphans are re-encoded and checked by exact length/SHA/descriptor identity, then
repeat file/directory barriers. Conflicting bytes remain refused. A failed step
leaves the old root, subject to the existing atomic-rename/final-directory-Sync
uncertainty; a visible unacknowledged root retries the required barriers.

No source transaction or mutation lock spans a visitor, encoder or root IO. The
same zero-queue Exporter lock and projection heavy-job lifetime cover preparation,
source pages, all sinks, publication and cleanup. Cancellation requests exit;
Close withdraws the owner and joins actual callback/publication completion.
Callbacks cannot call Close or re-enter a heavy query on that same owner.

## Selected Runtime maintenance

The selected disk branch now calls ExportDiskStep with explicit local limits:
16 pages, 256 attempt rows, 8 MiB cumulative source input and output; each page
still caps 160 rows / 4 MiB input/output. Existing finite partition, write/work
and one-minute hard caps remain; Runtime supplies its current 10-second context.
One tick performs one step. Later ticks continue from the root rather than
requiring the whole historical backlog to fit again. On failure, normal logged
retry retains the prior prefix; no Aggregate fallback is used. Replica keeps its
existing guard against exporting the native durable tail.

These are fixed local work controls, not calibrated physical RAM/disk
reservations, guaranteed progress within every filesystem deadline, a throughput
promise or an accepted production capacity configuration. Encoder/work budgets
can still refuse a caller's chosen step; callers must choose mutually sufficient
source/output limits rather than treating an IO failure as prefix success.
Collector catch-up, native authentication and fresh owner construction retain
their own existing costs and boundaries; this does not make full startup bounded
by one archive step.

Whole-history resumable exact verification, date-retention coverage for late
completions, resumable old catalog migration/retention, orphan/scratch reclamation,
CLI/default disk startup, shared fees, trusted recovery and Linux/production
acceptance remain unfinished. The published root is archive coverage metadata,
not content/key-set authentication, a trusted restart snapshot or trim permission.
