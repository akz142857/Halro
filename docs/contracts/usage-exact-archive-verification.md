# Exact verification of selected disk Usage archives

Status: local implementation, 2026-10-03. I-08 remains ongoing. Default Usage
is Aggregate. The actual offline CLI now builds fresh authenticated Ledger/Usage
disk references and uses this verifier; see the
[offline command contract](usage-offline-verification.md). Default startup,
prune, retention and migration remain incomplete. Actual
[offline compact](usage-offline-compaction.md) now uses the same fresh reference
and compares the existing archive before append and its final target afterward.

## Range and evidence

`Exporter.VerifyDiskArchive` compares every native-derived attempt from genesis
through the captured archive root's H. It does not infer a retention floor
from the first listed partition. A missing first partition, suffix or entirely
empty catalog therefore cannot manufacture a smaller comparison range.

`VerifyDiskArchiveAfter` instead compares the explicitly selected `(after, H]`.
The caller must establish `after` from its native/retention authority. All listed
archive rows must belong to that interval. The API does not authenticate the
caller's retention choice. Date-based retention with late completions can create
holes in native sequence order; this contiguous interval API does not implement
that policy. A future retention root must explicitly describe its coverage and
be verified against the native facts under that policy before deletion.
Partition `Date` metadata is not a retention authority or a reason to skip native
rows in this verifier. Validating date-based coverage remains a separate prerequisite
before any pruning can rely on that field.

Both methods require a selected fresh DiskProjection built from authenticated,
role-allowed native facts. They accept paged catalog v1 and legacy JSON catalogs
without rewriting or migrating the latter. A first bounded-value walk establishes
the complete legacy header and digest; a second walk streams individual entries,
inheriting a missing per-file schema only from the validated root schema. Files
may precede the schema/header fields in JSON. Both passes use the same captured
descriptor and compare the complete document; no `Manifest.Files` is collected.
Archive H may precede the projection's current head; later native appends are
excluded from comparison. The report separately names `AfterSequence`, archive
`ThroughSequence`, the actual earliest listed sequence and the actual
`SourceHead`; it never substitutes archive H for the physical source watermark.

The job walks immutable catalog pages or individual legacy entries and stores canonical row JSON in a private
bbolt index keyed by SHA-256(EventID), with independent sequence and path indexes.
The full EventID and every schema-expressible row field must match the selected
native reference. Hash collisions cannot produce a false match. Repeated IDs,
sequences or partition paths fail closed. Per-file schema, increasing sequence,
first/last position, record count, token totals and cost totals also validate.
Schema 3–8 rows compare against the appropriately narrowed current native row.
Actual historical Parquet files with missing columns are supported through the
reader's schema conversion; newer fields are not fabricated for old rows.

The captured root's bytes/header/identity are checked again before success.
Any error, cancellation, limit, retirement or cleanup failure clears the entire
report. A report proves relative row correspondence for the selected interval;
it is not D-03 content/key-set authentication, a restart snapshot, a native
anchor, proof of all current facts being archived, or compaction/prune permission.

## IO and decoder boundary

Each partition is copied with a fixed 64 KiB buffer into a private immutable
spool, checking the captured regular descriptor, exact length, SHA-256 and named
identity/size/mtime. One spool is open at a time. Only the validated private bytes
reach a decoder; preflight and decoding cannot race a mutation of the original
partition between reads.

NDJSON uses a bounded value parser and strict known-field decoding, with a 3 MiB
row limit independent of the 64 KiB catalog descriptor limit. Parquet footer
framing is checked before typed library decoding: at most 1 MiB, 65,536 Thrift
nodes and depth 16. Metadata has at most 128 schema elements, 128 row groups and
64 columns per group. There are at most 65,536 pages per file; page headers are
at most 64 KiB / 1,024 Thrift nodes. Compressed and uncompressed pages are at most
8 MiB, with a 256 MiB aggregate uncompressed declaration cap per file. Declared
value/dictionary/row counts and V2 level/null lengths also validate.

Preflight verifies actual uncompressed, Snappy or Zstd page output before the
library's normal read. Snappy decoded length must match; the private single-worker
Zstd decoder limits memory/window and output capacity. Other codecs and oversized
legacy partitions are explicitly refused; bounded normalization of such history
is still required, with no silent fallback to an unlimited decoder. The actual
reader is synchronous, uses 32 KiB read buffers and 16-row visitor batches;
unsupported-schema/data panics produce a refused job.

Default hard job caps are 65,536 files, 1,048,576 rows, 256 MiB per copied file,
1 GiB archive reads, 256 MiB physical scratch index and one minute. Callers may
lower these caps. The shared archive read budget includes both paged root walks
(three root walks for legacy catalogs, including final revalidation),
catalog pages, partition copying, preflight and decoder re-reads across every
file. Random reads conservatively charge the requested buffer, including a
request extending beyond EOF. The native reference has the separate finite
`DiskArchiveLimits` source budget. The index sets bbolt's `MaxSize` before opening,
checks logical growth for both rows and paths, and rejects a cap below the DB's
initial page allocation before creating it. Physical index exhaustion is reported
as `ErrDiskUsageLimit`.

The legacy path uses the same decoder, row/path/sequence indexes, explicit native
interval, private spool and physical Close join as the paged path. Empty catalogs
and missing first partitions do not authorize a smaller interval. Tests check
file identity, content, mode and modification time before/after both successful
and refused legacy jobs; scratch files are removed. Creating/removing scratch can
change the root directory's modification time. This API is not a filesystem-wide
read-only inspector; the offline caller still must authenticate native authority
before creating any scratch or exporter. See the [offline prerequisites evidence](../verification/performance/2026-10-03-usage-offline-prerequisites/README.md).

These are finite job controls, not shared resource reservations or calibrated
peak resident memory. Footer metadata, dictionaries, decoded pages, strings,
codec workspaces, JSON encoding and mmap still need I-03/D-04/V-02 capacity work.
A whole archive exceeding a job cap fails closed; resumable whole-history
verification without early success remains unfinished.

## Ownership and maintenance

One zero-queue Exporter mutation lock and the selected projection's existing
heavy-query slot cover root reads, index/spool creation, decoding, native
comparison and final cleanup. Source DB readers/mutation locks are released
before partition IO and callbacks. `DiskProjection.Close` withdraws the owner
and joins actual physical work, including reads that outlive context cancellation.
The job closes the index and removes only its matching private scratch directory
before releasing that lifetime. Close/removal errors cannot yield a success
report. Process crashes can leave scratch; restart reconciliation is unfinished.

The caller must retain its data-directory ownership for the whole operation.
The verifier never modifies the published root, catalog pages or data partitions.
It does not acquire that external lock or export/trim/delete native facts. Default
disk Runtime startup, the remaining maintenance CLI paths, resumable migration/retention, reclamation,
shared fees, authenticated restart and Linux/production acceptance remain open.
