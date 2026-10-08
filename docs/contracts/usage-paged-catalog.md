# Usage paged archive catalog v1

Local implementation/verification in progress, 2026-10-03. I-08 remains ongoing;
default Usage still selects Aggregate. No actual production data is migrated by
the implementation tests.

## Format and compatibility

The fixed root remains `manifest.json` and has manifest schema 9, with separate
`catalog.version=1`, `catalog.head`, `last_sequence`, `complete_prefix` and
`files:null`. Row schemas remain 3–8. Schema 9 deliberately makes older binaries
refuse the root; keeping schema 8 with an unknown catalog field would let them
misinterpret files:null as empty history and overwrite its archive position.

The root head has a SHA-256, exact page length and cumulative page/file counters.
Pages are immutable content-addressed files at
`catalog/v1/page-<lowercase sha256>.json`, each with at most 64 descriptors and
64 KiB total bytes. A page refers to its predecessor and records its descriptors
in source order. Cumulative counters must decrease exactly by one page and the
page's descriptor count; there is no recursive traversal or visited-history map.
Root/page hashes are content checks relative to the selected root, not native
source authentication, a MAC of root/key sets, or deletion-resistant trust.

`ReadArchivePosition` returns constant-size root metadata. Valid paged roots have
a 64 KiB total cap. The legacy JSON reader still streams old schemas 3–8 one
bounded descriptor/value at a time, with its existing 1 GiB catalog cap.

The constructor validates all reachable pages once before returning an owner.
The scan has a one-minute context and one-million-page ceiling. Ordinary append
checks the old head, then writes only new pages plus a new root. It relies on the
already validated immutable ancestors remaining unchanged during this owner's
lifetime; a deep historical mutation is detected by explicit full validation or
reopening, not by re-reading every old page on every append. D-03 authentication,
role-generation publication and persistent trust remain unfinished.

## Migration and publication

`MigrateArchiveCatalog` streams a captured old manifest twice, compares raw
SHA/scalar metadata and named file identity, and emits bounded pages. Implicit old
file schema attribution becomes its original manifest row schema. It preserves
old frontier, complete-prefix flag, container formats and physical data files.
The migration caller owns the data-directory exclusion/lifetime. The method
serializes with Export/Prune and uses a finite timeout and 1 GiB actual write cap.
It is not yet a delivered CLI migration workflow or resumable migration journal.

The selected disk Runtime's `ExportDisk` now publishes this paged layout. On its
first legacy export it migrates the old directory while appending the finite new
source entries, publishing one new root only after all source partitions and
catalog pages succeed. Later jobs append pages referencing the previous head;
no historical manifest rewrite is performed. Empty successful cuts retain the
root and repeat manifest durability barriers, after checking the head still
exists and validates.

Catalog pages use private temp write, file Sync, no-overwrite Link and directory
barriers for v1, catalog and root. A matching content-addressed orphan repeats
content/length/identity validation and file/directory Sync. Different bytes are
refused. Root visibility is one synchronized-file atomic Rename followed by root
Sync. A failure before Rename cannot activate draft pages; a failure of the final
root directory barrier may leave a visible unacknowledged root, as with the
previous publisher. Successfully built unreferenced pages are retryable orphans;
reclamation and interruption-safe retained-page deletion are separate work.

## Bounded readers and verification

`ReadArchiveFiles` returns at most one physical catalog page or a subpage, newest
page first. That order is not presented as row chronological order. Its opaque
HMAC cursor contains the next page reference/index and captured archive frontier,
uses an independent random 32-byte Exporter key, and is scoped to that owner
lifetime. Limits are 1–64 descriptors; cursor input is capped before base64
allocation. Each page's declared length/digest, canonical encoding, counters,
file descriptor schema/path/range/hash and frontier are checked. Array count is
capped before decoding ManifestFile structs. Errors return no partial page.

Appends leave old pages available, so a previously issued cursor can finish its
old frontier without including future files. Current retention does not delete
these pages; cursor-aware reclamation/leases remain required before retention.
No HTTP/CLI pagination contract is exposed by this internal reader yet.

`ValidateArchiveCatalog` checks complete reachability, counters and descriptors
without retaining history. `VerifyArchiveFileChecksums` additionally hashes each
physical data file with a 64 KiB buffer and captured descriptor length, validates
identity/mtime, and caps the whole job at one minute/16 GiB data/one million page
reads. Neither method proves canonical row sets, EventID uniqueness, full row
schemas/totals or native reconciliation. Bounded exact disk-backed verification
and resumable larger verification remain required; these methods must not be
credited as legacy Verify/Reconcile equivalents.

Legacy LoadManifest, Snapshot Export, Verify/Reconcile and Prune explicitly
return `ErrArchivePagedCatalog`, preserving the stronger old operation contracts
rather than silently returning an empty archive. Old default Aggregate workflows
continue to use old-format manifests. Migration/rollback compatibility and CLI
commands are unfinished O-01/O-02 work.

## Remaining switch conditions

Large-backlog bounded complete-prefix scheduling; exact canonical verification;
resume/reclaim protocols for migrations, orphan pages/data and retention; disk
Usage index trimming; shared RAM/disk/confirmation fees; native authenticated
restart/role behavior; Linux encoder/runtime capacity and production acceptance.
The default Runtime switch is still withheld by those incomplete conditions.

Before default disk startup is implemented, Runtime explicitly refuses a paged
root before restoring/replaying Usage into Aggregate. Two failed Open attempts
must release physical candidate owners and the directory lock and preserve the
archive. A default Aggregate runtime may not report readiness while its export
and retention methods cannot operate on this format. This temporary startup
guard is a remaining migration/switch condition, not a usable production mode.

## Exact row verification follow-up

The selected fresh owner's [exact archive verifier](usage-exact-archive-verification.md)
now supplies bounded canonical-row/native-set comparison independently of the
catalog-only and checksum-only APIs. Its default interval starts at genesis;
a caller-selected retained interval must be explicit. Missing catalog entries
cannot establish a retention floor. Scratch indexes enforce duplicate EventIDs,
sequences and paths without a resident historical map. This does not complete
D-03 authentication, the legacy CLI, resumable whole-history verification or
retention/reclamation, and does not authorize a native trim.

## Finite prefix publication follow-up

Selected Runtime maintenance now publishes [finite complete-prefix steps](usage-archive-steps.md)
and resumes at the root through a new Exporter. Root publication uses the
receipt's explicit ThroughSequence rather than the later actual source Head.
Receipt/source-fence/range checks also apply to strict exports. This closes the
local whole-backlog source-budget retry problem; it does not complete resumable
migration/verification, retention coverage, reclamation, shared fees, authenticated
recovery or the default disk startup switch.
