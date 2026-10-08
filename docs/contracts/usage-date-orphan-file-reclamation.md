# Explicit orphan data-file reclamation

Local implementation, 2026-10-03. `ReclaimRetainedDateOrphanFile` handles one
explicit `date=YYYY-MM-DD/<leaf>.ndjson` or `.parquet` name. The offline Standalone
entry is `ReclaimUsageDateOrphanFile`. It needs no caller-provided descriptor and
can inspect a recreated object after the original old page chain has disappeared.
It is a component of orphan reclamation; bounded directory inventory, orphan
catalog-page processing and old-root obligation completion remain unfinished.

## Independent authority and actual discovery

The operation consumes a fresh published-root comparison and current-reference
index from the same fresh projection owner. It repeats the independently keyed
[original policy receipt](usage-date-policy-authority.md) and exact original
native-prefix authentication. A saved data/page Ready or directory name supplies
no permission. The old activation receipt is preserved throughout this operation.

The path must be canonical, at most 4096 bytes, exactly one valid UTC date
partition and one visible leaf with a supported extension. Parent traversal,
absolute/nested paths, backslashes, reserved hidden temporary names and unsupported
extensions are refused. The date must precede the active policy cutoff. Current
data-path membership is checked before target decoding, so a late current object
that reuses an old expired name remains protected.

The actual pinned parent and regular nonempty file are opened and checked. Size
must fit the physical file limit before copying. A context-aware bounded copy
creates a private spool and computes SHA-256 over actual bytes. NDJSON decoding
uses bounded values; Parquet uses the existing bounded metadata/compression
preflight before library decoding. Filename extension chooses a decoder, and a
container mismatch refuses the target rather than selecting another format.

Fixed counters derive one descriptor: actual schema, strict ascending sequence
range, row count, token/cost sums, partition date and digest. Every row must have a
supported homogeneous schema, match the partition date and be at or before the
original authenticated prune baseline. Empty/malformed content, duplicate or
reversed sequences, mixed/unknown schemas and counter overflow are refused.
No historical row array, date map, directory listing or saved discovery cursor
is retained. Discovery target, parent and spool actual Close/cleanup succeed
before the candidate is handed to the native verifier.

## Actual native comparison and physical transition

The derived descriptor remains an untrusted candidate. The existing
[physical data-file verifier](usage-date-file-reclamation.md) reopens the target,
checks the frozen digest, decodes a fresh private spool and compares every row
with the fresh native-derived projection at its exact sequence. Readable historic
schemas compare only columns that schema could express. Genuine late native rows
beyond the original prune baseline are protected even when their dates are old.

Immediately before unlink, checksum, actual file/parent/root identities, native
head/window, original marker/comparison and current named references are rechecked.
The actual pinned parent removes only the leaf, then directory Sync and actual
file/parent Close complete. No native facts, current root, policy receipt, catalog
pages or shared physical-space fees are removed by this API.

Missing target/parent retry repeats fresh authorization and actual available
directory Sync/Close. Its returned Entry is empty, rows and logical bytes are
zero, and `AlreadyMissing=true` makes no claim about historical content. A real
unlink returns the actual verified descriptor and logical removed size. Any late
error withdraws both descriptor and reclamation report; committed unlink is not
undone. Every result keeps `ObligationPending=true`.

## Limits and App boundary

The existing file/row/read/native/index/time limits apply to the entire job.
Defaults are 4096 rows, 256 MiB per file, 1 GiB aggregate physical read IO,
64 MiB native-value IO, 1 MiB index IO and ten seconds. Discovery and independent
verification share the same aggregate budgets, so the maximum file size alone
does not promise acceptance of all decoder/re-read costs. The two bounded decode
passes trade additional IO for separating discovery from deletion authority.
Physical temporary disk use and shared fees still require capacity integration.

The App uses the same exclusive data-directory lock, independently unlocked
native keys, fresh projections, readonly Metadata actual Close before mutation,
and final native/member/Metadata/owner-cleanup fences as descriptor-based reclaim.
Its complete native construction/authentication retains the one-minute deadline
and is not resumable initialization. HA and persisted-member configuration
downgrade are refused. CLI/default Runtime/HA wiring, directory inventory,
obligation completion, next policy activation, backup/rollback recovery and
Linux/production acceptance remain open.

[Finite live data inventory](usage-date-orphan-reclamation.md) now supplies
bounded directory entries and actual batch budgets on one fresh owner. Its EOF
is an observed stream boundary with unknown/outside-policy objects preserved,
not an authenticated orphan-free or old-obligation completion receipt.
