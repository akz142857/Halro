# Selected disk Usage archive publication

Status: local implementation/verification in progress, 2026-10-03. Default Usage
is still Aggregate. This continues I-08; it does not complete its catalog,
retention, verification, resource or recovery dependencies.

## Publication

The strict `Exporter.ExportDisk` consumes the selected fresh disk owner's attempts in native
sequence order. Consecutive completion-date runs within one bounded source page
become independent schema-8 Parquet or NDJSON partitions. An older date appearing
later in the native stream cannot move publication past unexported lower rows.
The source freezes H, covers the complete interval above the previous archive
frontier, and only its successful completion can publish a new manifest at H.
H includes non-attempt facts after the final attempt. `complete_prefix` records
this distinction; old schemas 3–8 and the historical highest-attempt manifests
remain readable. The flag is derived metadata, without content authentication,
key-set deletion protection or trusted restart/compaction authority.

The selected disk Runtime now uses [finite complete-prefix steps](usage-archive-steps.md).
`ExportDiskStep` keeps the real source Head separate from its successful
ThroughSequence, publishes only that finite prefix, and resumes at the committed
root on the next job. The strict whole-source API remains available.

A failed strict whole-source budget/decode/encoder/write leaves the old manifest. The
atomic manifest rename is the visibility point. If its final directory Sync
fails, a new manifest can be visible without an acknowledged publication;
idempotent retry repeats manifest file and directory barriers. Success does not
mean every possible filesystem IO is interruptible by the context deadline.

A same-name orphan is never overwritten. The bounded partition is re-encoded
into a fresh private temporary file; the existing regular file's exact length,
streaming SHA-256 and descriptor/named identity must match these candidate bytes.
The matching orphan repeats file Sync, partition directory Sync and root
Sync. Different bytes, including a different encoding of otherwise similar rows,
are refused conservatively. The fixed `usage-bounded-v1` filename namespace keeps
this retry contract separate from historical unlimited partition writers.

Publication serialization includes legacy Export and archive Prune mutators.

The [window authority](usage-window-authority.md) now follows native-relative
finite publication. A missing root starts at zero; an existing prefix must
match a prior verified/published witness. Constructed root bytes and the actual
temporary inode bind a private receipt after rename. It becomes usable only
after barriers and actual cleanup succeed. A failed publication retains one
pending receipt for exact-root retry, with repeated file/directory sync;
unknown roots and reopening do not inherit it. This does not enable default
Runtime disk retention, date coverage or native deletion.
Disk export uses a zero-queue TryLock. Physical disk owner retirement joins the
source callbacks and final manifest publication. There are no DB readers or
mutation locks spanning encoder, partition or catalog IO.

## Finite working sets

The default local limits are:

- Complete source: at most 4,096 pages, 1,048,576 rows, 128 MiB input and 128 MiB
  serialized source output; each page at most 160 rows and 4 MiB in/out.
- Per partition: at most one source-page date run, 16 MiB encoded file; at most
  256 rows are accepted by the publisher even with another caller's source limits.
- Parquet: one finite row group, explicit 32 KiB page and write buffers, a private
  ZSTD codec with one worker, disk-backed column-page buffers. No page buffers or
  codec owner are retained by Exporter between partitions. At most 128 column
  scratch descriptors per active partition, each with a 16 MiB write cap.
- Actual partition writes: 256 MiB/job. Actual column scratch, descriptor staging
  and manifest writes share a 1 GiB/job cumulative work cap. Descriptor staging
  also has a 64 MiB cap. These caps count writes rather than claiming preallocated
  disk space or calibrated peak resident memory.
- Whole job: at most one minute (Runtime currently supplies a 10-second context).
  A backlog exceeding a strict complete-job budget fails closed. Selected Runtime
  scheduling now uses one finite step per tick: 16 pages, 256 rows, 8 MiB source
  input/output; other publisher caps remain. Data/sink/work/write/time failures
  still refuse a job and cannot authorize a partial successful prefix.

Shared resource reservations and actual encoder peak/RSS/Linux capacity are
unfinished I-03/D-04/V-02 work. These finite controls alone do not prove that a
production envelope is sufficient.

## Catalog and maintenance boundaries

Startup and Runtime position-only readers stream old JSON catalogs for schemas
3–8. The selected disk publisher now uses [paged catalog v1](usage-paged-catalog.md):
a fixed schema-9 root and immutable content-addressed pages capped at 64 files/
64 KiB each. Row schemas remain 3–8. Existing catalogs migrate by a bounded
stream; later appends write only new pages and root metadata. A fixed signed
cursor serves one physical page without retaining history.

The root upgrade deliberately blocks older <=8 consumers. Current legacy
LoadManifest/Export/Verify/Reconcile/Prune explicitly reject the new layout.
Bounded catalog validation and data checksum scanning are available. A selected
fresh-owner [exact row-set verifier](usage-exact-archive-verification.md) now
compares an explicit native interval using a disk index and bounded decoders.
Whole-history resumable verification, migration/retention recovery and orphan
reclamation remain unfinished. The compatibility and ownership details are in
the paged catalog contract.

Runtime's selected disk export branch has no Aggregate fallback. Replica methods
refuse export of their local durable tail. The selected disk runtime currently
holds window/archive pruning and native compaction closed, since those protocols
are unfinished. Ordinary default Aggregate maintenance remains active. There is
no public mode switch and no production release/acceptance claim.
