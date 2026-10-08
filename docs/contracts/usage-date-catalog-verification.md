# Finite date-candidate row-set comparison

Local implementation, 2026-10-03. `VerifyRetainedDateCatalogStep` compares a
completed [date candidate](usage-date-catalog-candidate.md) with the caller's
selected fresh DiskProjection. It implements finite candidate comparison, not
root activation, native/member authentication, saved-projection trust or deletion.

## Two finite phases

The archive phase walks immutable candidate pages with a fixed reference and
page index, decodes bounded physical partitions, and indexes canonical schema-
narrowed row digests. Every descriptor binds its UTC partition path/date and
every decoded row's completion date. Actual checksum, increasing sequence,
min/max/count/token/cost summary and schema/identity fields must match. Separate
event, sequence and path trees refuse duplicates. Expired candidate descriptors
are refused; the candidate is expected to contain only dates at/after the
caller's explicit date.

The source phase scans the entire native-derived attempt interval `(0, H]` in
finite sequence pages. It requires every retained attempt, checks every listed
row's complete schema-narrowed digest, counts permitted expired omissions and
rejects unmatched extra archive rows at EOF. H remains the frozen live root's
processed sequence, including trailing non-attempt records; dates never choose
a sequence floor. A projection whose attempt window has already been pruned
cannot supply this genesis comparison.

A successful complete published-root comparison now also restores an ephemeral
window permission after independent policy authentication and real cleanup.
Candidate/general scoped comparisons do not. Window pruning subsequently makes
this query source unsuitable for full-history date reclamation; it requires a
fresh untrimmed reference. See [the startup/window contract](usage-startup-window.md).

The API has no accumulated historical files/rows/read-byte job limit. Limits
apply separately to each call. Defaults: four files, 4,096 rows per atomic
partition, 128 source rows/one source page, 256 MiB physical partition and
per-partition decoder reads, 1 GiB total archive reads and index IO, 4 GiB index
space, ten seconds. Source retains its own input/output/page byte limits.
Limits are explicit local controls; index space can be configured up to 1 TiB,
and per-call duration at most a minute. Physical admission/calibration is open.

A partition is still an atomic bounded decoding unit. Over-limit legacy files
need a compatible bounded repack or a larger supported per-file limit; this API
does not implement resumable rows inside one oversized file. Existing codec
preflight limits also apply. Between files, the call reserves the partition's
decoder allowance and worst fixed-node index work before entering it. Between
source pages it caps rows by the remaining index-read allowance. A smaller valid
work cap saves partial progress; it is not an unbounded full-history traversal.

## Comparison index and ownership

One private scratch directory and index belong to the Exporter. The compressed
binary radix index uses 160-byte nodes and at most 256 branches plus a leaf in a
lookup. Roots, offsets, extent and a random MAC key are fixed-size process state;
there is no mmap, history-sized cache, array, map or freelist. Disk extent grows
with indexed rows/files, within the caller's explicit index-space cap.

Each node's contents and offset are covered by the ephemeral MAC. Lookup checks
strictly increasing branch bits and authenticated leaves, including negative
membership. A saved index is never reopened as trusted data; new verification
sessions always create a fresh private index. This MAC protects the disposable
comparison structure only and grants no native or D-03 authority.

Each step has zero-queue writer/heavy-query admission. Actual root, state,
partition/spool, codec, scratch Sync/Close and error cleanup remain inside the
accepted operation's owner lifetimes. Cancellation does not shorten a blocked
physical callback. Permanent Exporter Close joins the call and closes/removes
its idle private scratch; retirement, stale source, IO and ownership failures
withdraw the returned progress/report. Directory/index replacement is refused;
a foreign replacement directory is preserved. The copying helper now propagates
its source Close and failed spool-cleanup errors to all exact-verifier callers.

## Checkpoint revalidation

One canonical checkpoint, `.archive-date-verify-v1.json`, at most 8 KiB, records
the exact build-checkpoint digest, source head, phase, page/index, native position,
constant counters and deterministic work-unit count. It is synced, closed,
ownership-checked, renamed and directory-synced by the existing state publisher.
Root/build/source/window changes refuse it; a reset is explicit.

Persisted progress is untrusted. Reopening, changing checkpoint bytes or selecting
a different fresh source generation creates a new comparison index. Finite calls
recompute the saved prefix and compare the complete deterministic state at its
recorded boundary before extending it. Revalidation exposes no successful report.
Changed cursors, counters and apparent Ready cannot skip required native rows.
Restart can therefore redo historical IO; trusted fast restart is not supplied.

Ready means candidate row-set comparison reached full EOF relative to that fresh
projection. Actual scratch Close/removal precedes the Ready receipt; Ready retains
no cached verification shortcut. Another call, even in the same process, must
reconstruct the saved comparison. Ready is not publication/window/native trim
permission. Neither comparison nor reset changes the live root, files or epoch.
`ResetRetainedDateCatalogVerification` removes only the reserved checkpoint and
this process's owned scratch, refuses symlinks and leaves the build checkpoint.

## Authority and remaining work

The caller must independently authenticate native/role/Metadata before choosing
the fresh projection and recheck that authority before activation/report delivery.
Actual App reference callbacks test Standalone, Primary durable, Replica applied,
fresh generations and final member/native/Metadata/cancellation refusal.
The separate [activation API](usage-date-catalog-activation.md) now consumes a
single-use process comparison proof after independent App source/Metadata checks.
The comparison step itself does not publish a root and has no production CLI.

The existing whole-reference native construction/authority job still has a total
deadline; this cut does not implement resumable native authentication/construction.
Standalone logical activation and persistent old-root obligations are described
separately. Current-reference-safe unlink/resume, crash orphan cleanup, shared
physical fees, default disk
Runtime, D-03, HA migration and Linux/production acceptance remain unfinished.
I-07/I-08 remain ongoing. [Evidence](../verification/performance/2026-10-03-usage-date-comparison/README.md).
