# Single expired archive file reclamation

> The original-baseline authorization defect was reproduced on 2026-10-03 and subsequently repaired locally with a [native-bound authenticated policy receipt](usage-date-policy-authority.md). Unsigned legacy obligations fail closed. See [original failure](../verification/performance/2026-10-03-usage-date-data-reclaim/README.md) and [repair validation](../verification/performance/2026-10-03-usage-date-policy-authority/README.md); CLI/default Runtime and production acceptance remain unfinished.

Local implementation, 2026-10-03. `ReclaimRetainedDateFile` reclaims one caller
selected derived partition after logical date activation. The offline
Standalone entry is `ReclaimUsageDateFile`. This is the data-file operation
used by [finite old-root data cleanup](usage-date-data-reclamation.md). Catalog
page deletion, orphan cleanup and old-root obligation completion remain open.

## Authority and scope

The caller supplies an explicit UTC retention date and independently authenticates
the native/role/Metadata scope. The operation consumes a fresh process-owned
published-root row-comparison proof from the same fresh DiskProjection, and uses
the process-owned current-root reference index. Root position, raw SHA, inode,
directory, activation obligation and comparison checkpoint must still match.
Saved Ready/phase, a descriptor, filename sequence, or absence from an index
does not grant deletion authority. The current root must name neither the target
path nor omit a required late expired native row above the original prune baseline.

The target's date must precede the policy date and its maximum sequence must be
at or below the original prune baseline. Its actual rows must exactly equal fresh
native projection point reads, narrowed to the partition's supported schema.
The decoder checks row schema, increasing sequence, UTC completion date, row
count, minimum/maximum sequence, token/cost summary and checksum. Native billing
facts, projection rows, roots, catalog pages and the activation obligation are
preserved. An old descriptor need not be trusted to identify a historical owner:
the actual file must independently satisfy every check.

## Finite physical operation

Defaults are 4,096 rows, 256 MiB file size, 1 GiB aggregate read IO, 64 MiB native
point-read bytes, 1 MiB reference-index IO and ten seconds. Supported limits are
1–1,048,576 rows, file bytes up to 256 MiB, reads 2 MiB–1 GiB, native bytes from
the maximum native value through 128 MiB, index IO 1 MiB–1 GiB and at most one
minute. Codec preflight/decoder bounds also apply. An oversized atomic partition
is refused; finite repacking remains a separate requirement.

Writer and reclamation admission have no queue; accepted readers cause Busy.
The projection heavy-job and physical lifecycle permit cover all IO and actual
Close. Each admitted call consumes its comparison proof even on failure. A
busy call has not entered that consumption boundary. Exporter/Projection Close
permanently withdraw admission and join the actual operation; cancellation cannot
release a blocked physical callback or its retained file handles.

Parent traversal uses `os.Root`, rejects symlinks and checks the actual archive and
parent identities. Two traversal handles remain between components; opening the
next component or final recheck can briefly overlap three. The target is a
regular file opened through the pinned parent. A private spool supports bounded
decode/native comparison; its actual Close and ownership-checked removal precede
unlink. The held target is hashed again immediately before unlink, and its
content/identity, parent chain, current-root references and source bindings are
rechecked. Current root and obligation are synced before any destructive action.

The archive epoch advances conservatively before removing the leaf through the
pinned parent handle. Parent Sync, absent-leaf verification, final scope checks
and all real Close results precede success. Archive writer exclusion and the
offline data-directory lock are required ownership assumptions: this does not
provide atomic compare-and-unlink against an unrelated process that ignores them.
JS/Plan9 are refused because the required `os.Root` confinement is unavailable.
Only the documented local host gates have been exercised.

## Retry and final authorization

An already missing leaf or parent performs no deletion, returns zero rows/bytes
and retains the obligation after fresh authority and durability checks. Parent
absence does not mask actual handle Close errors. A failure after actual unlink
withdraws the entire success report; the missing name can be retried with fresh
authorization. It never rewrites an old file or erases its durable obligation.

The App wrapper authenticates fresh native and Metadata references, constructs
the current-root comparison and reference index, and independently revalidates
native/order/member/Metadata before the actual operation. Actual readonly
Metadata Close precedes unlink. Post-operation source/Metadata identity and
position checks plus physical cleanup precede report delivery. Configured HA and
persisted-member configuration downgrade are refused. It remains a standalone
API without a new CLI command or default Runtime/HA activation.

`LogicalBytesUnlinked` records only the removed name's logical size. Missing names,
unlink, open descriptors and hard-link aliases do not prove physical block release
or permit shared fee refunds. RAM/disk accounting, admission/calibration, full
native construction resume, cleanup-generation rebase, page/orphan cleanup and obligation
completion remain required by I-03/I-07/I-08. The App's complete offline reference
operation still has a one-minute budget; it is not a resumable full-history job.

[Local evidence](../verification/performance/2026-10-03-usage-date-file-reclaim/README.md).
