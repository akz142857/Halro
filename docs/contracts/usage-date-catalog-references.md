# Finite current-root reference index

The page tree also gates [finite original-page reclamation](usage-date-page-reclamation.md).
It protects shared current pages and is rebuilt under the fresh appended root;
the independent signed next-hop record carries irreversible old-chain progress.

> The original-baseline authorization defect was reproduced on 2026-10-03 and subsequently repaired locally with a [native-bound authenticated policy receipt](usage-date-policy-authority.md). Unsigned legacy obligations fail closed. See [original failure](../verification/performance/2026-10-03-usage-date-data-reclaim/README.md) and [repair validation](../verification/performance/2026-10-03-usage-date-policy-authority/README.md); CLI/default Runtime and production acceptance remain unfinished.

Local implementation, 2026-10-03. This supplies the named-path reference part of
date reclamation. It does not unlink data/pages, finish an old-root obligation,
authenticate native/role/Metadata, mint deletion/window authority, or refund fees.

## Frozen root and bounded construction

`PrepareDateCatalogReferencesStep` requires schema 10 and the caller's explicit
matching UTC date. It captures the actual root position, raw-byte SHA, inode and
directory identity. Each call walks finite immutable catalog pages and indexes
every listed canonical data path and catalog-page path in separate private radix
trees. The root's file/page counts must agree with the reconstructed chain; duplicate
paths, bad page hashes/structure, invalid dates and coverage are refused. It reads
descriptors, not the data partitions; presence/absence means named-path membership,
not content/row verification or physical inode ownership.

Defaults are four pages, 1 MiB root/state/page reads, 64 MiB index IO, 4 GiB index
space and ten seconds per call. Supported limits are 1–16 pages, read bytes from
two maximum pages plus one 8 KiB checkpoint through 8 MiB, index IO 16 MiB–1 GiB,
index space through 1 TiB, and at most one minute. Source reads and actual node IO
consume the respective caps. Before entering a page the operation checks enough
remaining input bytes and worst-case fixed-node work for the entire page. Smaller
valid caps stop between pages and persist completed work. Index-space exhaustion
refuses the result and closes the private scratch rather than reporting Ready.

The index uses the existing 160-byte MAC-bound nodes and at most 256 branch levels.
Roots, offsets, extent and ephemeral key are fixed-size process state; historical
nodes are on disk without a mapped DB, heap map, cache, array or freelist. Only one
reference scratch and one row-comparison scratch can exist on an Exporter. Shared
RAM/disk accounting, statfs admission and production calibration remain open.

## Untrusted checkpoint and process-owned Ready

One canonical `.archive-date-references-v1.json`, at most 8 KiB, stores frozen root
position/digest, next reference and page/file counters. It is file-synced, actually
closed, ownership-checked, renamed and directory-synced. Input/checkpoint/owner
errors or late cancellation/retirement withdraw progress and close/remove the
process-owned scratch. Foreign replacement directories are preserved.

Saved progress is untrusted. Reopen or changed checkpoint bytes creates a fresh
private index and recomputes the saved prefix in finite calls. The complete
deterministic reference/counter state must agree at its saved boundary before
extension or Ready. Even an apparent saved Ready reconstructs every current path;
it never reopens a saved index file or skips history based on a cursor/counter.
Revalidation returns no ready root or membership permission.

Ready retains the fresh process-owned index for bounded point queries. An unchanged
process session can reuse it; actual root/checkpoint/index/directory checks and
durability barriers still run. Append/root identity/digest/policy changes refuse
stale progress. `ResetDateCatalogReferences` explicitly removes only this reserved
checkpoint and owned reference scratch, preserving roots, data and immutable pages.
It also conservatively withdraws any in-process date activation proof.

The private point-query helper runs only under the caller's writer/reclamation
lifetime. It checks a physically current root and exact frozen binding, canonical
relative path, checkpoint digest/owner, directory and authenticated index lookup.
Any uncertainty is an error, never an absence proof. A lookup reads only bounded
root/state bytes and at most 257 nodes. The caller must propagate all errors and
actual closes; this helper grants no right to use an absent path as an unlink target.

## Physical ownership and next deletion phase

Writer admission has no queue and lasts through actual IO, Sync and Close. Exporter
Close permanently refuses admission and joins accepted jobs before closing both
private indexes. Cancellation cannot release a blocked physical callback. Owned
scratch cleanup preserves a foreign replacement owner and propagates Close errors.
The shared date-checkpoint publisher now rechecks directory identity and permanent
retirement before rename for construction, comparison and reference callers.

Data/page cleanup must combine this index with a fresh independently
authenticated native/role/Metadata scope and completed current-root row comparison,
explicit caller policy, actual reader exclusion, safe parent-path ownership,
actual file identity/content, finite untrusted cleanup replay and durable unlink
barriers. Named-path absence alone does not exclude aliases or prove file ownership.
File numbers, an old descriptor, missing paths, saved Ready/phase, or index existence
cannot replace those checks. Physical space/fees can be returned only after actual
release is evidenced. Orphan handling and old-root obligation completion remain open.

The [single expired data-file operation](usage-date-file-reclamation.md) and
[finite old-root data cleanup](usage-date-data-reclamation.md) now combine these
checks for explicit/old-root partitions. Saved data progress replays the old
descriptor prefix and requires eligible processed names to be missing. They keep
catalog pages and the obligation, without supplying physical fee refunds.

I-07/I-08 remain ongoing. [Evidence](../verification/performance/2026-10-03-usage-date-references/README.md).
