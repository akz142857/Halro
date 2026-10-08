# Pricing history selection and cold audit

`SelectDeploymentPriceVersion` reads the native deployment/time index. It does
not call `ListDeploymentPriceVersions`, build a whole timeline slice, or retain
price versions in its audit cache. A fixed 4,096-slot table retains synchronization and replaceable audit
markers. It retains neither deployment identifiers nor per-deployment clocks.
Slot collisions share exclusion; the full deployment SHA-256 digest and durable
revision must both match before reusing an audit marker. Current Gateway/Admin
callers hold one public pricing gate at a time; nested acquisitions across
identifiers sharing a slot are unsupported. Collision/load performance still
requires deployment calibration.

Cold auditing copies at most 64 rows / 1 MiB per page, then releases the bbolt
reader before decoding or private comparison-index IO. One legacy/wide row may
use up to the native journal's 8 MiB payload cap; size is checked before cloning
or decoding. Audit checks every version's validation, deployment, record ID,
canonical effective-time index binding and version uniqueness. Canonical index
binding makes duplicate effective times impossible without a refused index
inconsistency. Duplicate UInt64 version numbers use an exact disposable disk
radix set, with at most 64 branch steps and fixed MAC/read/write workspace.

Schema 42 adds the authoritative `deployment_price_revision` bucket. An absent
key means revision zero for the migrated baseline; migration creates an empty
bucket without walking price history. The native transaction recorder tracks
price record and deployment timeline mutations and increments each affected
deployment exactly once after all merged callbacks succeed, before native append
and database commit. Moving a record between deployments invalidates both.
Ordinary callers cannot rewrite the fence. Missing/malformed counters, overflow
and whole-root deletion refuse the transaction before native append.

Every copied audit page and completion checks the same deployment revision.
Candidate selection checks the durable revision in its own read transaction,
including on warm cache hits. Unrelated Metadata, pins and other deployments may
continue writing. Same-deployment mutation returns `ErrPricingRecoveryChanged`
when it interrupts auditing; a stale warm marker triggers one fresh audit, with
no unbounded retry. Revision zero is compared explicitly rather than treated as
an unset fence. Replica applies the recorded fence with its price operations in
one database transaction, so a warmed marker cannot survive a newly applied
price fact. The fence detects changes, not content authentication or rollback.

The schema gate prevents an older writer from opening schema 42 as current.
Primary/diagnostic opening requires the exact schema; an older Replica projection
is not silently migrated and pricing reads without the fence fail closed.
Whole-file restore migrates before publication and starts a fresh journal epoch.
The existing logical business-pricing digest excludes this separate fence bucket
and retains its schema-41 semantics; archive file hashing still covers all
Metadata bytes. A synthetic schema-41 encrypted archive round trip and atomic
migration interruption/retry are covered locally. This does not prove a mixed
version HA rolling upgrade or a released historical archive compatibility matrix.

Creation, scheduled cancellation and proposal adoption hold the deployment
audit mutex through commit and marker invalidation. Readers cannot load an old
marker into a newly committed timeline across those paths. The existing public
shared/exclusive pricing gate still protects selection through durable Lease;
the audit mutex does not replace it or change durable high-water/pin rules.
Native seek validates a candidate's raw size, record/deployment/time binding
and domain content, including canceled rows it passes.

Clock observations live in one freshly created, private, process-local disk
index. Its compressed 256-bit Merkle radix tree has at most 256 branches per
path and a 64 MiB working file cap. A resident root MAC authenticates child
references, keys and observation pairs, including absence. Restoring an older
valid leaf/branch or deleting records cannot satisfy that live root. SHA-256
binds deployment identities; slot-index collisions do not alias clock values.
Selection time and the monotonic elapsed offset from this process's origin move
together only for a newer selection. A cold Store starts a new index, matching
the prior in-memory clock's restart semantics; this is not a reusable snapshot.
The file needs no durability sync because its live root and origin are never
resumed after restart. Durable native price pins/high-water keep their existing
journal/commit/confirmation path.

The index refuses new identities at its working limit while retaining existing
observations. Corruption/IO failure poisons that live owner. If observation merge
fails after a native prepared-pin commit, Prepare returns no usable price or
intent, and the Gateway's existing accounting-error path stops provider dispatch.
The committed prepared fact remains for authoritative-reference recovery;
prepared pins without a Ledger lease are removed by that existing recovery.
Concurrent capacity checks can still commit a prepared fact before a subsequent
merge discovers exhaustion, so this error is not a database rollback claim.
Store admission spans clock creation/read/merge; public pricing gate holders own
Store admission through release. Close revokes new calls, waits actual admitted
work and gate release, closes the clock descriptor, and removes only matching
owned file/directory identities. Directory/file replacements are preserved and
reported. SIGKILL orphan inventory and shared physical fees remain open.

The audit creates an exclusive private comparison file and always closes it
before removing its owned inode and empty scratch directory. Cancellation or
cleanup error refuses the result. Store lifetime covers the whole public audit
operation; Close cancels its context and joins actual cleanup. A read-only
diagnostic Store uses OS temporary storage and does not create derivatives in
the source directory. The comparison index has a 64 MiB temporary disk cap;
exhaustion is an explicit working-limit error, not a corrupted-price verdict.

With an explicit shared RAM budget, every Standalone/Primary/Replica Runtime
configures pricing on its unpublished Metadata Store before pricing consumers.
One dedicated Recovery lifetime claim precedes the fixed slot table and clock
owner (structural minimum 1 MiB). Selection, Prepare and cold auditing acquire
an IO claim before allocating or reading their working set (structural minimum
32 MiB); public selection and its cold audit share one claim. Unknown/pressured
admission refuses before clock creation or native pin writes. Existing budget
configurations must supply both pricing envelopes and IO class capacity. The
normal pool must fit one HTTP envelope plus one pricing IO envelope and two
claims. HA Recovery now fits nine owners, including the pricing lifetime.

Cancellation does not refund an admitted call while it still runs. Store Close
joins actual calls/gates, closes the clock descriptor and clears retained pricing
references before refunding lifetime RAM. Cold audit refunds only after its actual
comparison-file Close succeeds. An unsuccessful physical Close retains its claim;
a matching-inode cleanup error after successful Close does not retain a RAM-only
claim. This makes no claim about disk/page-cache retirement. The explicit sizes
are structural floors requiring measured calibration, not allocator/RSS limits.
Other pricing mutations, returned objects and complete-list consumers are not
automatically covered by this IO reservation.

The production Admin price list now uses `DeploymentPriceVersionsPage`, not
`ListDeploymentPriceVersions`. HTTP defaults to 50 rows and accepts 1..100;
records additionally share the 1 MiB raw page target, with one native wide row
within 8 MiB. It first reuses/establishes the full timeline audit, so off-page
conflicts refuse a page. Rows, active price, next scheduled price and the latest
uncancelled scheduled blocking price come from one read transaction. Lifecycle
status uses that call's selected_at; it is not derived from only visible rows.

A fixed process-private MAC key signs a stateless cursor binding deployment,
durable revision and native time key. Changed facts return pricing_page_changed;
malformed/cross-deployment/reopened-Store cursors return pricing_page_cursor.
Each page must retain the same facts revision, while live statuses can advance
with time. The MAC key is erased after Store admission joins. This continuation
is not a reusable authenticated content snapshot. Summary scans remain scalar
but can walk cancelled tails; their worst-case latency still needs calibration.

The console uses the same-read summary even outside the visible page, polls for
the next scheduled transition, replaces pages, and evicts inactive price-query
pages with gcTime zero. It restarts on cursor invalidation rather than appending
a mixed prefix. The legacy complete-list method remains for compatibility/tests;
the compatibility whole-history methods remain outside this bounded API contract.
Startup pending audit intents and their metrics now use the bounded protocol below.

Local tests and growth samples are in
[initial verification](../verification/performance/2026-10-04-pricing-stream-audit/README.md)
and [deployment revision verification](../verification/performance/2026-10-04-pricing-revision-fence/README.md),
and [bounded identifier-state verification](../verification/performance/2026-10-04-pricing-state-bounded/README.md),
and [shared pricing RAM verification](../verification/performance/2026-10-04-pricing-ram-budget/README.md),
and [native Admin pages verification](../verification/performance/2026-10-04-pricing-native-pages/README.md),
and [pending audit recovery verification](../verification/performance/2026-10-04-pricing-audit-bounded/README.md).
Samples distinguish retained heap from cumulative allocation and do not prove
RSS/cgroup bounds, Linux latency or production capacity.

Remaining scope includes RAM fees for other pricing consumers, physical scratch fees, profiled audit allowances,
remaining scalar history latency and returned-object/other-consumer RAM fees,
other history-sized pricing collections, clock orphan retirement and Linux capacity acceptance. The
existing `ListDeploymentPriceVersions` complete-slice API remains unchanged;
this contract is not a claim that all pricing consumers are bounded.

Schema 43 adds `pricing_audit_control/pending_count`, an authoritative UInt64
maintained by the transaction recorder. All intent Put/Delete/delivery operations
contribute a net delta; the one resulting counter operation shares the native
frame and atomic projection commit with its facts. Replica applies the recorded
counter with the confirmed facts, without recalculating it. Ordinary callers
cannot edit the control bucket or remove the intent root. Counter overflow,
underflow, missing/malformed state, or failed callbacks refuse the native append.
Every recorded write checks counter readability, including zero-delta writes.

The schema-42 migration scans borrowed raw records individually and writes one
fixed counter. It creates no history-sized index or dirty-page set; JSON temporary
work is limited by one native 8 MiB row. It still holds one O(history) schema
transaction and is not the final O-01 resumable migration or a fully priced schema
job. Migration failure rolls back schema, counter and history together. Restoring
schema 41/42 archives preserves the logical pricing/pending hashes and backfills
the counter before publishing a new native epoch; this is not mixed-version HA
upgrade acceptance.

`RecoverPendingPricingAuditIntents` is a trusted, quiescent startup operation.
It freezes the upper physical EventID key and source transaction, then reads
pages of at most 64 rows / 1 MiB raw bytes, allowing one native 8 MiB wide row.
Delivered prefixes advance the cursor at the same limits. Readers exit before
JSON decode or audit callbacks. Full validation of pending rows, matching keys,
count and the unchanged initial transaction must complete before delivery; a
malformed late row or forged zero count does not append an audit receipt.
Delivered legacy rows retain the previous exemption from current domain validation.

Delivery first retries events still present in the native Audit retry window,
then processes the remaining pending events in stable physical EventID order.
The retained-ID predicate is a scheduling hint only; Audit Append verifies the
full persistent JSON content. Startup must prevent new audit appends through this
priority pass. This prevents a previously durable, undelivered receipt from
being evicted by thousands of new receipts. Audit decoding preserves exact JSON
numbers and retry comparison uses serialized content, so uint64/time/pointer
metadata reconstructed after restart does not create a false payload conflict;
different exact content still refuses the same EventID. This does not introduce
historical membership lookup beyond the existing 4,096-event retry window.
Original OccurredAt remains unchanged. New recovery receipt ordering differs
from the old whole-slice timestamp sort; ledger/HA consume actual append sequence.

Each delivery retains Append → checkpoint → durable delivered-marker ordering.
Interrupted recovery resumes remaining pending rows. Final count must be zero:
a new pending record inserted behind the cursor cannot grant readiness. This is
not a general concurrent-drain API, authenticated saved snapshot or final D-03.

With shared budgets, any nonempty intent history holds one dedicated Recovery
workspace of pricing_io_ram_bytes (minimum 32 MiB) through prevalidation, priority
retry, new delivery callbacks and actual job exit, even when pending_count is
zero. Fixed pricing/Governance lifetimes remain separate. Cancellation/Store Close
join callbacks before refund; callbacks must not synchronously close their own
Store. General budgets reserve three Recovery claims and HA ten; HA RAM adds this
workspace to 3*(candidate+queue+read)+reader+governance_log+pricing_lifetime.
These are calibrated logical envelopes, not allocator/RSS bounds. No extra
scratch file is used here; physical persistent/log/mmap costs remain unfinished.

`halro_pricing_recovery_pending_intents` reads the one counter and omits its
series when the counter/store cannot be read; it does not load intents or emit
false zero on an error. A scrape does not reauthenticate the source history.
The complete-list API remains for compatibility/tests, with no production caller.
Startup still scans O(history) two or three times and performs per-event durable
work; this stage bounds retained collections, not total recovery latency.

Schema 44 adds an empty authoritative pricing_proposal_revision bucket; missing
per-deployment keys are baseline zero and no proposal history is rewritten.
The recorder binds every proposal mutation's old/new deployment identity and
bumps each affected deployment once in the same frame/commit. Direct revision
edits, root removal, malformed/exhausted revision and failed merged callbacks
refuse append. Replica applies the recorded fence only at its confirmed cut.

Schema 45 adds authoritative `pricing_proposal_order` (deployment → fixed
44-byte created-time/ID-hash position → proposal ID) and `pricing_proposal_count`
(deployment → UInt64). Every recorded primary Put/Delete maintains the old/new
index and count immediately in the same transaction, including moves between
deployments and reordering. The deployment revision still advances once after
all merged callbacks. Direct index/count writes and root removal refuse append;
missing old entries, malformed counts and underflow/overflow roll back the fact
and index together before the native frame is appended. Replica applies exactly
the recorded operations at its confirmed cut, without independently rebuilding.
Creation reads this count instead of decoding all historical proposals; an
idempotent replay remains available at the 10,000-per-deployment capacity. Other
idempotency capacity Stats() reads remain; the complete write path is not claimed
to have constant history cost.

Migration 45 atomically reserves the native buckets and schema, and sets (or
preserves) the durable epoch-pending gate. It does not build an index from a
projection that may lag its authenticated journal. Attach first authenticates and
reconciles the old journal, then clears intermediate indexes/counts in at most
64-key/1-MiB transactions (only empty child buckets are dropped), validates owned
source pages after reader release, and writes at most64 rows/1MiB per transaction
(one native8MiB wide row allowed). It also recounts pending audit intents from
the post-reconciliation source with finite readers and a final scalar write:
pre-schema43 native frames carry no counter operation.

The schema number may already be45 during construction, but the persistent
pending gate blocks proposal pages, creation and public pending-count reads;
metrics omit an unavailable count. It remains until the epoch file and DB
position/ready commit have completed. Primary exact-schema Open independently
refuses any unpublished migration; Replica journal attach refuses an incomplete
seed. This Standalone preparation does not replace the ordered HA schema
boundary. An earlier prepared epoch marker survives the schema45 upgrade,
including the epoch-file-before-position crash window.

Cross-process intermediate contents are never trusted from a bare cursor/count:
retry clears and rebuilds finite blocks from the source. The file-before-position
finish path performs the same rebuild before activating its prepared epoch.
Facts/revisions and the old authenticated journal remain intact across build
errors; committed intermediate blocks stay hidden and retryable. Public encrypted
Restore of schema41/42/43/44 archives completes this protocol before publication,
preserving facts, proposal revisions and pending intents.

These blocks limit this index's dirty-page work per transaction, not all startup
memory or physical disk use. Native-tail recovery now commits one complete
authenticated frame and its applied position per transaction; Outcome-definition
epoch preparation now clears and builds finite blocks under the same pending
gate. Older schema migrations and large atomic native operations still need
separate capacity handling. Per-frame fsync may increase replay time; full
authentication scans remain a separate limit. Replica ApplyThrough now uses the
same per-frame atomic commit and performs complete validation before any frame
commit; failure returns the committed position for retry. See
[Replica recovery evidence](../verification/performance/2026-10-04-replica-metadata-blocks/README.md) and
[recovery-block evidence](../verification/performance/2026-10-04-metadata-recovery-blocks/README.md). Shared
migration fees, space admission, authenticated prepared-state reuse, offline CLI,
progress probes and Linux capacity/maintenance acceptance remain O-01/I-03/D-03
work. See [bounded migration and crash evidence](../verification/performance/2026-10-04-pricing-migration-bounded/README.md).

Admin proposal listing uses DeploymentPriceProposalsPage. On the first read of a
deployment revision, a cold audit compares the complete primary source against
its index and count, then validates every indexed reference back to its primary
fact. This catches omitted/spurious index entries, wrong counts, missing facts
and malformed off-page digests before returning any items. Physical readers
release at 64 rows /1 MiB (one native8 MiB wide-row allowance); decoding happens
after reader release. The primary scan freezes its physical upper key and checks
the deployment revision at every boundary, preventing unrelated suffixes from
extending the scan. Existing pricing recovery callers keep full-source TxID
fences with no upper bound.

A fixed4096-slot process marker caches only SHA256(deployment), revision and a
loaded flag. Slot collisions replace the marker and force a cold audit. The cache
relies on native store integrity and recorder-owned revisions; it does not prove
absence of out-of-band same-revision tampering or replace D-03 authentication.
After the cold audit, subsequent pages seek the native deployment index and walk
backwards without scanning or retaining history. CreatedAt stays descending,
with deterministic SHA256(ID) ties and pre-epoch UTC order. A page decodes at most
1..100 rows /1 MiB (one native wide row allowed), with one borrowed lookahead;
final revision revalidation prevents publishing facts across a mutation.

The stateless 156-character proposal cursor binds deployment, proposal revision
and the fixed44-byte order position, using the process key and a separate
endpoint byte. Other-deployment changes do not invalidate it; own mutations,
reopening or passing a price-version cursor require restarting. It is a
continuation, not snapshot/content authentication. Errors return no partial page.
The call holds the configured pricing IO reservation through actual return/Close
join; returned objects/HTTP encoding and other consumers still need complete
resource ownership. No query scratch disk is created. Cold audit latency and
cumulative allocations remain O(all proposals + selected deployment proposals);
ordinary warm pages have bounded row work. The HTTP/client API accepts explicit
next_cursor, does not accumulate pages, and returns503 on validation/storage
errors. See [native-index verification](../verification/performance/2026-10-04-pricing-proposal-index/README.md)
and the [earlier bounded-page evidence](../verification/performance/2026-10-04-pricing-proposal-pages/README.md).
The three full-list compatibility methods currently have no production Go
caller; their external consumers receive no automatic boundedness guarantee.
