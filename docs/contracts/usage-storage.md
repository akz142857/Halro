# Usage storage and retention

> Current selection, 2026-10-04: default Standalone Open uses fresh disk Usage,
> synchronous recovery and archive/window verification before readiness. Its
> periodic controller, export and public observers share that owner. Primary
> now selects role-bound disk Usage with a separate disk Ledger evidence owner;
> its business accounting remains State. Replica retains its applied-prefix
> Aggregate path. Earlier staged implementation notes
> below do not describe today's default selection. See [owner contract](standalone-usage-runtime-owner.md)
> and [current evidence](../verification/performance/2026-10-04-usage-default-runtime/README.md).
> I-07/I-08 remain ongoing; full-history IO, shared physical fees, trusted resume,
> full migration/backup and production capacity remain unfinished.

Original catalog pages now support [finite authenticated reclamation](usage-date-page-reclamation.md)
after actual data-job completion. Signed next-hop intent survives deleted old
heads; current references and the original policy receipt remain protected.

> The original-baseline authorization defect was reproduced on 2026-10-03 and subsequently repaired locally with a [native-bound authenticated policy receipt](usage-date-policy-authority.md). Unsigned legacy obligations fail closed. See [original failure](../verification/performance/2026-10-03-usage-date-data-reclaim/README.md) and [repair validation](../verification/performance/2026-10-03-usage-date-policy-authority/README.md); CLI/default Runtime and production acceptance remain unfinished.

Halro treats the Ledger WAL as the accounting authority. The live aggregate,
bbolt checkpoint, and Parquet files are rebuildable derivatives. Budget
admission and recovery never depend on Parquet.

The Collector queue stores wake notifications only, with no `ledger.Record`
payloads or borrowed event references. Observe is non-blocking; saturation marks
lag and drops a wake, never a durable fact. CatchUp streams the authoritative
suffix from the selected reducer's error-returning watermark. Read errors do not
choose genesis or another Aggregate. Queue depth, dropped wakes and lag remain
exported metrics; notification arrival during a scan retains a later wake.

One Run and two admitted catch-ups bound source/reducer IO and mutex waiters.
External CatchUp returns Busy when both slots are occupied. The single Run can
retain one pending wake while waiting for a real slot, cancellation or retirement;
it neither discards Busy wakes nor spins/retries through new goroutines. Each
CatchUp performs one source scan, checks the actual reducer semantic sequence,
and leaves concurrent notifications dirty for the next scan. Physical roll heads
may differ from the last semantic event. Source errors remain explicit and
lagging; this is not an autonomous repair/retry guarantee for source failures.

Retire permanently withdraws Observe/Run/CatchUp, cancels cooperative work and
joins actual admitted work and the Run before clearing borrowed owner references.
A caller timeout does not release slots or dependencies early. Retirement never
closes the borrowed source/reducer DB; terminal Runtime Accounting closure joins
the Collector before native/Metadata dependencies close. Apply lock waits and
legacy source IO are cooperative only and retain their slots until actual exit.

`NewProjectedCollector` accepts the same selected disk Ledger derivative; caller
authorization and source authentication are required. Fresh Replica construction
authenticates the entire native history but only reduces its frozen allowed
prefix, then retires the construction Collector before native adapter takeover.
It does not trust an existing DB or grant default Runtime publication. Default
primary/standalone still uses the resident Aggregate; wake-only admission does
not bound that historical state, storage IO or shared physical resource fees.

Standalone now selects a fresh disk Ledger owner for default accounting and
binds that same owner before resident Aggregate replay/checkpoint restore. Its
two-level startup recovery finishes and Usage catches up before publication.
The Ledger derivative is rebuilt from complete authenticated history on every
open; see [Standalone owner contract](standalone-ledger-runtime.md).

## Files

```text
data/
├── ledger/ledger.wal
├── usage/manifest.json
└── usage/date=YYYY-MM-DD/usage-<min-sequence>-<max-sequence>.parquet
```

Each Parquet row represents one settled Provider attempt. It contains stable
request/attempt/event IDs, Project/Key/Route/Provider/model IDs, token and
micro-USD totals, UTC timestamps, outcome/error class, latency, retry count,
and fallback count. Prompt, response, credentials, authorization headers, and
provider keys are not part of the schema.

Publishing uses temporary file creation, file `fsync`, atomic rename,
directory `fsync`, and then an atomic manifest commit. The manifest records
the schema version, exported Ledger sequence, per-file SHA-256, row count,
token totals, and cost totals. A crash before manifest commit can leave an
orphan file; the next compaction validates and adopts an identical orphan.

## The console checkpoint

The aggregate the console reads is checkpointed into `metadata.db` so a restart
replays only the tail of the WAL rather than all of it. It is stored as a head
plus a series of immutable record segments:

- the **head** (`meta/usage_checkpoint`) carries the watermark, the window's
  floor, the totals and histograms, the requests still in flight, the dedup
  window, and an index of the segments — nothing in it grows with the number of
  records held;
- each **segment** (`usage_checkpoint_segments`, keyed by a big-endian id) holds
  a contiguous run of attempts and request summaries in ledger order, and is
  never rewritten once sealed.

A checkpoint round writes the head, the segments it adds or replaces, the
segments the window has trimmed past, and the daily-rollup increment **in one
bbolt transaction**. A head that committed without its segments would name
records nobody can read, and a checkpoint that advanced without its increment
would leave the rollup describing a prefix of the WAL nobody can name.

A round rewrites the head and at most one open segment, so its cost follows what
arrived since the previous round rather than what the window holds. Restore
reads segments one at a time, refuses any checkpoint it cannot fully read — a
missing segment, a segment that disagrees with the head, records out of ledger
order — and drops records below the stored floor, which is what lets a partially
trimmed segment stay untouched until it can be deleted whole.

Both derivatives are cleared together whenever either is unusable; the Ledger
rebuilds them.

Reader-bound checkpoints carry `reducer_semantics=ledger-first-facts-v1`.
Default Standalone rejects an unmarked legacy checkpoint and clears its daily
rollup in the same reset before replay. This includes old duplicate-terminal
totals with no active rows. A marked checkpoint also cannot resume under plain
semantics. The first Ledger terminal remains the request summary; later attempt
facts update their accounting without reopening the request. Legacy per-lease
attribution requires explicit origin evidence from the same selected owner.

## Operations

Run these while the server is stopped. The commands acquire the same exclusive
data-directory lock as the server.

```text
halro usage compact --config ./config.yaml
halro usage verify --config ./config.yaml
halro usage verify --config ./config.yaml --after-sequence 12345
halro usage prune --config ./config.yaml
halro usage prune --config ./config.yaml --before 2026-05-01
```

`compact` builds a fresh authenticated disk reference, exactly verifies an
existing archive before append, and advances finite complete prefixes until its
whole frozen target is reached. It then exactly verifies the final archive and
rechecks independent native/Metadata evidence. Errors return no success report;
already published complete prefixes can be retried from their committed frontier.
The bounded JSON report names the root/count, starting and final sequences,
steps, newly written rows and verification scope. Missing/legacy catalogs stay
schema-8 streaming JSON so current default Runtime startup remains compatible;
existing paged catalogs stay paged. No historical file array is returned.
See [offline compaction](usage-offline-compaction.md) for hard job ceilings,
retained-history refusal, failure publication and remaining capacity work.
`verify` builds fresh disk Ledger/Usage references from independently authenticated
native facts and checks safe manifest paths, SHA-256, schema version, exact Event
IDs/row fields, sequence bounds, cost and tokens. It accepts legacy JSON and paged
catalogs without rewriting them, and Parquet or NDJSON partitions. Default scope
is genesis through the captured archive H; `--after-sequence` explicitly selects
`(after, H]` and is never inferred from missing partitions. It prints a JSON
reconciliation report; successful `missing`, `duplicates` and `extra` are zero.
The report also names the interval, actual Usage source head, role-allowed physical
Ledger head, full native head, role and ordering index. Primary selects signed
durable ordering evidence; Replica selects applied evidence and verifies Metadata
agreement. Authentication, final source/member checks, limits and cleanup failures
withdraw the report. See [offline verification](usage-offline-verification.md)
for authority, lifetime and capacity limits. `rebuild-summary` now authenticates
fresh disk references and publishes daily rows in finite transactions through a
two-slot derived root; its marker deliberately requires native replay at the next
Aggregate startup. See [offline summary rebuild](usage-summary-disk-rebuild.md).
`prune` and default Runtime Usage remain on their resident paths.

`prune` is deliberately explicit. Without `--before`, it uses
`usage.retention_days` (default 90). It commits a manifest excluding expired
partitions before unlinking those files. It does not delete the Ledger WAL;
WAL segment deletion remains disabled until segment rotation, backup pins, and
full cross-layer retention proofs are implemented.

## DuckDB examples

DuckDB is optional and is not linked into the Halro binary.

```sql
SELECT provider_id, provider_model,
       sum(cost_micros_usd) / 1000000.0 AS cost_usd
FROM read_parquet('data/usage/date=*/usage-*.parquet', hive_partitioning=true)
GROUP BY 1, 2
ORDER BY cost_usd DESC
LIMIT 20;
```

```sql
SELECT project_id, key_id,
       sum(provider_input_tokens + provider_output_tokens) AS tokens
FROM read_parquet('data/usage/date=*/usage-*.parquet', hive_partitioning=true)
WHERE completed_at_utc >= now() - INTERVAL 7 DAY
GROUP BY 1, 2
ORDER BY tokens DESC;
```

```sql
SELECT date_trunc('hour', completed_at_utc) AS hour,
       count(*) AS attempts,
       count(*) FILTER (WHERE status <> 'success')::DOUBLE / count(*) AS error_rate,
       avg(latency_millis) AS average_latency_ms
FROM read_parquet('data/usage/date=*/usage-*.parquet', hive_partitioning=true)
GROUP BY 1
ORDER BY 1;
```

## Fresh disk Usage projection candidate

`CreateDiskProjection` creates an exclusive, node-derived DB.
`NewDiskLedgerDerivative` binds the selected Ledger first-position/finalization
reader and current-event bridge before Apply. Each write hydrates a temporary
legacy Aggregate with one request/start/hour and constant counters. Attempts,
terminal summaries, filter indexes, active/start state, hourly/daily rows and
watermark commit in one transaction. Errors discard the scratch reducer and roll
back every row. Global active count uses persistent request existence changes,
not scratch map length. Daily dimensions preserve the earliest-200 plus `other`
semantics. Fixed SHA keys retain and check complete identities in their values.
No historical Aggregate/checkpoint or pending rollup map remains in this candidate.

Attempt and summary pages bind local cursor handles to generation, query and a
frozen semantic head. These rows are immutable. Each page caps its physical scan
at 256 rows, input/output content at 8MiB each, and decoded values at 3MiB. Raw
size is checked before decode; returned objects are independent and the reader
ends before caller output/IO. Sparse filters may return no items with a continuation.
Overwide input/output fails explicitly. Filter text is cumulatively capped before
encoding; encoded queries have a separate 1MiB cap. Single encoded content bounds
do not prove actual encoder, dirty-page/freelist/mmap or whole-process RAM peaks.

Stats and daily rollup point reads describe one current short View. Mutable rows
cannot reconstruct an old page head without versioned increments or frozen sources.
There is no cross-page historical aggregate promise.

Two fixed slots admit internal query preparation and reader operations, with no
queue. Saturation returns a busy error before query encoding/result allocation.
Encoding the bounded query happens inside its slot but outside the DB View. Slots
are released on failure and after View/copy; output aliases and response encoding
remain the caller lifetime and budget responsibility.

Close withdraws operations, joins transactions and reader callbacks, closes the
DB and clears references. The bridge and DB have explicit separate lifetimes:
owners close the bridge first, then the DB and remove only owned paths. Native
and shared Metadata remain caller-owned. Replica preparation uses fresh Ledger
and Usage DBs; it never consumes or resets old shared Usage content. Construction
does not select the default Runtime, authorize a trusted reopen, publish a
checkpoint/rollup generation, or grant readiness.

FailurePage now provides a bounded request-to-attempt join over immutable disk
rows. A local fixed-size cursor binds generation, complete query and frozen H,
and saves a pending summary, latest settlement, association position and predicate
decision. Empty pages can advance this join; consumers must keep Next until Done.
Every nonempty attempt filter must match the same attempt. Any attempt can match,
including a successful one; LastFailure independently describes the latest
settlement by sequence through H, or is nil when that settlement succeeded.
Settlements later than finalization but at or below H participate without changing
the terminal summary. BeforeSequence limits summaries, not attempt association H.
Empty/success terminal outcomes are excluded, matching the existing failure view.

The entire page shares scan/input/output bounds across its candidate Views. Index
and companion bytes are charged together; resumed summary and latest-attempt reads
are charged again. Candidates are decoded/copied under a bounded View; result
measurement/encoding runs after that View closes. Output exhaustion keeps a resolved
pending row for the next page. An individual row too wide, a corrupt/missing
companion, cancellation or an invalid cursor returns an error and zero page, with
no partial Next. Insufficient budget to advance returns limit error; callers may
retry their unchanged input cursor with larger page limits. Limits are not part of
the frozen query signature. Fixed metadata reads are outside row byte accounting.
Reader slots and the lifecycle RLock still cover internal result encoding; Apply
waits on that lock. Transaction-free encoding does not prove zero writer waiting
or a calibrated RAM limit. HTTP cursor serialization/authentication, outer API
page accumulation and response encoding still need their own bounded lifetimes.

Fresh format v2 adds immutable request-summary versions and a current version
point index. A version's applied sequence is distinct from Summary.Sequence:
active summaries retain Sequence=0, terminal summaries retain the original
finalization sequence. Changed active observations and finalization publish a
version/index in the same transaction as attempts and the Usage head. Unchanged
reservation/start events, historical duplicate IDs and late settlement do not
add a version or rewrite the finalized summary. All versions remain until this
generation retires; deletion needs retention/cursor ownership gates first.

RequestDetailPage captures current H and selects its summary through the bounded
version-head point index. Continuations pin the exact version key; a missing
selected version fails instead of falling back to an older or current summary.
Every page repeats that immutable summary and returns at most 100 attempts in
ascending settlement arrival sequence with an exclusive after position, through
the same H. Later settlement/finalization is absent from an old cursor and appears
in a fresh query. Page size can change while request/generation/cut remain fixed.
Zero-attempt requests still return their summary. Missing request returns Found=false,
Done=true; known current request without a version head is corruption. Complete
coordinated deletion of keys remains part of D-03 saved-content/key-set detection.

Summary/index bytes and copied attempts share the 256-row, 8MiB input/output and
3MiB single-value limits. Repeated summary reading and encoding are charged each
page. Fixed root metadata and constant absent-key existence probes are separate
from row byte accounting. Each summary/attempt copy ends its View before content
encoding. No attempt can fit means an explicit limit error; output exhaustion
after progress preserves the unconsumed attempt, and any error clears the entire
page. No same-after continuation is published. Owned decoded output survives View
closure, while caller retention/response encoding needs separate resource fees.
Fresh format v2 has no trusted reopen or implicit v1 upgrade path.

Fresh format v3 adds per-accounting-period `(ID, timezone version)` and completion
UTC-hour request quality. Only newly produced terminal summaries contribute, in
the same Apply transaction as their summaries, versions, facts and Usage head.
Requests and request errors retain the Dashboard definition (`Outcome != success`);
latency samples reuse requestLatencyMillis, excluding missing acceptance/nonpositive
duration and retaining a positive duration rounded down to 0ms. Late settlements
and original-ID physical duplicates contribute no additional requests or latency.

The exact latency index uses a fixed 44-bit binary count tree over nonnegative
duration milliseconds. Each sample writes 45 counters in each of two scopes,
with overflow/root/child-count conservation checks; failure rolls back both scopes
and every other derived row. This preserves exact nearest-rank P50/P95 rather than
replacing them with the existing daily rollup's approximate histogram. Counts and
range keys are fixed-width; the quality row retains and validates the complete
scope identity. All history stays on disk; physical growth and write amplification
remain to be measured and charged.

ReadPeriodRequestQuality and ReadHourRequestQuality each describe one current
View with its semantic watermark. A single scope takes at most 179 counted point
probes: one quality row and two selections of one root plus two child counters
through 44 levels. Empty/unknown latency scopes take three probes. Each key/value
and wide quality row is admitted to the shared row/input budget before decode;
result encoding occurs outside the View. Insufficient limits, malformed/missing
positive-sample tree paths, mismatched scope, cancellation or impossible duration
leaf returns zero result. Integer rank arithmetic avoids count-times-percent
overflow. These are foundation reads, not a complete Dashboard. Separate period
and hour calls do not establish a joint cut; a multi-hour Dashboard must add its
own coherent view and total cost/output gates. 179 is not the whole-dashboard
budget. Path checks do not authenticate the complete tree or key set (D-03).

Fresh format v4 adds exact Dashboard period attempt totals, all-group counters,
fixed top-five references, up to five anomaly sequence references, and an ordered
hour index. These rows commit atomically with attempts, existing request-quality
trees and the Usage head. Every group stays on disk: Dashboard does not apply the
daily rollup's 200-key/other folding. One settlement changes one group per each of
five dimensions. Nonnegative ranking measures only increase, so the old top five
plus that candidate yield the exact new top five, preserving numeric-descending
and complete-key-lexicographic-ascending ties. Each cache stores fixed hash
references; full identity and counters reside in group companions. Missing period
or an existing dimension's top is refused on a later settlement rather than
silently resetting history. Whole coordinated deletion remains a D-03 concern.

ReadDashboard combines Today, Hourly, exact P50/P95, active count, watermark,
calls/cost/errors top five for all dimensions, and recent anomalies. One admitted
reader RLock spans short Views and internal result encoding, establishing a
single current cut while Apply waits. Result encoding/measurement occurs outside
Views; finite scope/index-key encoding and companion identity checks still occur
inside some Views. Nothing retains a DB transaction after return. This is a
current-generation observation, not an HTTP cursor or a role-publication barrier.

The entire call shares explicit limits: at most 256 returned hours, 65536 counted
probes, 8MiB key/value input and 8MiB encoded Dashboard output, with the existing
3MiB value ceiling before decoding and fixed root metadata separately bounded.
Individual fragments are measured before accumulation, then the complete JSON
size is checked including framing. Repeated group/quality reads are charged;
179 probes remains a single quality scope, not the whole query. The signed UTC
hour index seeks directly to now-minus-seven-days, handles hours before 1970, and
preserves the legacy lack of an upper now bound. Too many future hours, insufficient
rows/bytes, malformed companions, cancellation or retirement produce an error
and zero Dashboard, never a truncated or partial successful response.

Estimated tokens are retained in hourly buckets and period totals. Estimated cost
is retained in Today and breakdowns; Hourly preserves the legacy zero estimated
cost field. Late settlements add attempt totals/groups/anomalies without changing
finalized request counts or latency; physical duplicate IDs advance only the head.
Fresh v4 has no trusted reopen or automatic older-format migration. This remains
a candidate API; the default Runtime and HTTP handlers still use Aggregate.

Fresh format v5 adds cumulative rollup versions and a fixed 51-byte range index.
Index order is ten-byte accounting-label prefix, numeric timezone version,
dimension and complete-row identity hash. Complete PeriodID and dimension key
are validated from the version; inclusive Start/End comparisons use the complete
PeriodID, so a long label that shares the end-date prefix is excluded correctly.
There is no bbolt wide-key regression for legal long period/model identifiers.
The source-page order is physical, not the Summary's final group-key sort order.

Each rollup mutation publishes current cumulative row, new applied-sequence
version, index first/last sequence, previous version's successor and Usage head
in one transaction. Row and Sequence remain immutable; only NextSequence adds
the successor witness. A continuation selects the newest value through its H,
and refuses a missing selected middle version: an older candidate whose successor
is at or below H cannot stand in for it. A later successor above H does not
change the selected cumulative value. Paired scope/count presence and current
row/range presence prevent a later settlement from silently restarting missing
single-row history or the first-200 admission count. Coordinated missing content
and complete index/key-set authorization remain D-03 work.

RollupPage returns at most 100 items, with one fixed local generation/query/H/after
cursor. Limit and page budgets can change while the predicate and cut remain
fixed. A query selects one or two known dimensions, or one non-total dimension's
one value; it supplies valid inclusive accounting dates over at most 3660 days.
Final day/month/year output ceilings belong to the complete candidate Summary
query described below. Histograms retain the existing approximate-summary definition and
request-only dimensions. Native order still admits the first 200 keys per
period/TZ/dimension, then accumulates the remaining contributions into __other__.

One admitted reader slot/RLock covers preparation, short Views and item encoding
for the entire page. Views end before result JSON encoding; finite index-key
encoding/identity checks still occur inside Views. Index and selected-version
reads are separately charged before decode to shared page limits of 256 probes,
8MiB key/value input, 8MiB sum of encoded item values and 3MiB per source value.
Fixed root metadata and page framing are separate; this is not a whole HTTP
response allowance. Unselected dimensions and rows created after H are still
charged and advance the cursor. Budget exhaustion can yield an empty page with
progress; no progress returns a limit error. Output exhaustion leaves the current
selected association unconsumed. Corruption/cancellation/retirement returns zero
page and no continuation. No transaction survives the page or caller return.

As-of versions are retained until generation retirement. This does not freeze
the cost of scanning later inserted index associations; the complete candidate
Summary job now has aggregate work/deadline gates. Shared scratch-disk/ordering fees, versions,
successor rewrites, mmap/dirty pages and disk growth need resource calibration.
Fresh v5 has no trusted reopen or automatic older-format upgrade. The default
HTTP handler still collects resident/persisted rows and has not switched to this
source. The candidate completes bucketing, span/timezone changes, selected group
sorting/folding, checked error propagation and whole-job ownership, without
default Runtime/HTTP publication.

This continues I-07. Default disk Summary selection and HTTP query/cursor delivery,
window/retention, bounded Collector, Parquet/manifest migration, default Runtime,
resource fees and D-03 saved-content authorization remain required.
Local evidence: [Usage disk projection](../verification/performance/2026-10-03-usage-disk-projection/README.md).
Failure query evidence: [Bounded disk failure joins](../verification/performance/2026-10-03-usage-failure-query/README.md).
Request detail evidence: [Same-cut paged disk request detail](../verification/performance/2026-10-03-usage-request-detail/README.md).
Quality evidence: [Exact request-quality point reads](../verification/performance/2026-10-03-usage-exact-quality/README.md).
Dashboard evidence: [Same-cut bounded disk Dashboard](../verification/performance/2026-10-03-usage-dashboard/README.md).
Rollup source evidence: [As-of bounded rollup range pages](../verification/performance/2026-10-03-usage-rollup-range/README.md).

### Candidate complete Summary and default HTTP response boundaries

The fresh v5 `ReadSummary` API freezes source H across all rollup pages and reduces
complete groups on an exclusively owned temporary bbolt DB. Output holds at most
400 day/36 month/10 year buckets and 100 fixed hash/rank top references plus one
folded tail; full-key lexical ties and rank-before-fold semantics remain. All
metric additions/subtractions and percentile/ranking arithmetic are checked.
The whole job limits cumulative pages/probes/source/output bytes, logical scratch
key/value reads/writes, physical scratch file, period identities and cooperative
duration. Actual cleanup precedes final context/retirement publication checks.
Close joins the complete job, including cleanup; foreign paths are preserved.
Limits are explicit candidate guardrails, not physical shared fee acceptance.
[Contracts and local evidence](../verification/performance/2026-10-03-usage-summary-job/README.md).

The default HTTP Summary still uses the old Aggregate/persisted-row reducer. It
now admits one active request with zero queue before parsing/CatchUp, holds that
permit through final encoding and actual response write, and permanently
withdraws/joins it during business drain or Close before closing Metadata/native.
A one-minute context is cooperative: actual ignored cancellation or blocked Write
keeps the permit and dependencies held until real exit.

Raw query is limited to 1MiB before parsing. A typed response preflights all
escaped variable strings and caps observed bucket counts, timezone changes at
4096, groups/labels at 101 and filter fields at two; exact encoded response plus
newline must fit 8MiB before success headers. This bounds final response encoding
after reduction; it does not bound the old reducer's historical maps/scans.

Resource labels now query only returned project or deployment group keys, never
ListProjects/ListDeployments/implicit provider normalization. Folded/missing keys
remain unlabeled; other dimensions/ungrouped reports get an empty label map.
At most 101 supplied identities/1MiB key bytes, 8MiB per raw Metadata row and
16MiB total raw key/value bytes before copy/decode; encoded label map at most
1MiB. Only ID/Name decode outside short Views, full ID must match the point key.
Any label error clears the entire optional label map; numeric data is still
served with ID fallback. Labels are current Metadata observations, independent
of the accounting cut. All these are logical limits and need physical calibration
and shared fees; pending versions/retention/archive/default disk generation/D-03
and Linux/production acceptance remain.

## Selected disk public queries (local, 2026-10-03)

Attempt/failure/detail handlers now route through the Runtime Usage query view.
A selected disk owner uses signed fixed-prefix continuation, bounded complete
detail and real HTTP response retirement/join. Empty physical pages with a next
cursor remain pageable in the console. Default startup still selects Aggregate;
maintenance/export and other observers have not migrated. See the
[query contract](usage-public-disk-queries.md) and
[verification](../verification/performance/2026-10-03-usage-public-queries/README.md).

### Public selected disk Summary wiring (2026-10-03)

Public Summary now chooses the selected disk owner, with complete wire semantics
and no old Metadata rollup merge or Aggregate fallback. The default startup
still selects Aggregate. Fixed exclusive scratch, authority path checks,
whole-job source/reduction ceilings, cleanup-before-response and actual HTTP
response join are documented in [the public query contract](usage-public-disk-queries.md).
The selected disk branch has local equivalence, failure, existing-path and
blocked-response tests. This advances I-07; startup reconciliation, default
owner assembly, observers, checkpoint/export/retention and shared fees remain.

### Selected disk observers (2026-10-03)

Dashboard, onboarding, system status and metrics now read the selected disk
owner, including coherent first-value counters, successful-finalization traffic
proofs, latest verification context and explicit metrics read failure. Disk
observers do not construct Snapshot or read legacy resident/checkpoint history.
The [public query contract](usage-public-disk-queries.md) records whole-job
ceilings, actual response join and the separate metrics retirement boundary.
Default startup and maintenance still select Aggregate; this does not complete
checkpoint, archive/retention or physical owner migration. Onboarding's bounded
scan can exhaust before proving readiness on large history, and configuration
list APIs retain their existing materialization cost.

### Independent native anchor and bounded archive producer (2026-10-03)

Native Ledger deletion protection now advances independently of Usage CatchUp
or legacy checkpoint success. A selected disk view skips old checkpoint/rollup
writes; Replica never advances the anchor to its native durable tail. The fresh
disk owner now exposes a frozen complete-interval archive producer with finite
pages, total work limits, transactions ended before sink IO and physical Close
join. [The contract](usage-archive-source.md) records the precise receipt and
failure boundaries. The old Export/manifest/CLI paths remain resident; bounded
encoding, atomic complete-prefix publication, paged catalogue, orphan retry,
checksums/retention and physical fees remain I-08 implementation work.

The selected disk Runtime now has a complete-prefix finite partition publisher:
[Usage disk export](usage-disk-export.md). It streams the legacy JSON catalog
without retaining Files, and preserves old Parquet/NDJSON/schema compatibility.
The paged catalog, bounded offline verification, backlog scheduling, retention
and shared fees still need implementation before the default Usage switch.

The selected disk exporter now publishes [paged catalog v1](usage-paged-catalog.md)
with a fixed schema-9 root and immutable finite pages; data row schemas remain
3–8. The root-only position reader, bounded signed page reader and streamed legacy
migration are implemented locally. Exact row reconciliation, resumable retention,
CLI migration/rollback and shared capacity acceptance are still required.


## Fresh disk window format v6

The [UTC date verifier](usage-archive-dates.md) adds an explicit retention
predicate and the offline `verify --retain-from YYYY-MM-DD` command. It scans
the complete native archive prefix, requires all retained-date rows, and still
checks listed expired rows exactly. Partition descriptors and paths must match
each row's UTC completion date. It never derives a sequence floor from missing
files or grants full-history window/native deletion authority. Date-retention
bounded unlink and default Runtime selection remain open; logical Standalone
root publication is described in the separate activation contract below.

[Archive reader lifetime](usage-archive-lifetime.md) now uses finite zero-queue
admission through actual file closes and a private exclusive reclamation gate.
Signed archive continuations bind an ephemeral cleanup epoch; append preserves
old H, reclamation invalidates even empty continuations. Runtime/offline Close
joins readers before dependencies and directory-lock release. These are local
ownership controls; bounded date unlink and shared physical fees remain unfinished.

[Finite date candidates](usage-date-catalog-candidate.md) now save fixed
construction progress and rederive the saved prefix across bounded calls after
reopen. Schema 10 expresses date/baseline coverage and is preserved by append;
catalog/exact/native-scope readers reject invalid coverage. Candidate Ready does
not activate the live root or authorize deletion. Default Runtime selection,
current-reference-safe unlink and full-history exact verification resume remain open.

[Standalone date activation](usage-date-catalog-activation.md) now consumes a
single-use process comparison proof after independent App native/Metadata checks,
persists one bounded old-root obligation before logical root publication and
rechecks authority after publication/physical cleanup. Reopen requires a finite
full current-root comparison and actual candidate-page ancestry, then repeats
barriers while preserving normal appended roots. Saved Ready/phase supplies no
authority. Both owner Close calls join actual publication; errors withdraw the
report while preserving committed derivatives and pending obligations. This is
a Go API with reclamation pending, not CLI/default Runtime/HA activation or
permission to unlink data, trim native facts or refund physical fees.

[Current-root reference construction](usage-date-catalog-references.md) now
indexes all canonical data/page names in finite calls, bound to the physical root
and explicit date. Saved-prefix/Ready replay rebuilds a fresh private fixed-node
index before membership queries; append/owner changes refuse stale state. One
reference and one row-comparison scratch can coexist; permanent Close joins work
and physically closes both. This is named-path membership, not native/content
verification, alias/parent/inode ownership, unlink or shared-fee authority.

[Single expired data-file reclamation](usage-date-file-reclamation.md) separately
combines fresh published-root row comparison and native point reads with current
references, actual file/parent ownership, pinned-parent unlink and Sync/Close.
The offline Standalone wrapper independently authenticates and rechecks source
and Metadata before and after unlink. Late errors withdraw reports; missing-file
retries retain the old-root obligation and return zero logical bytes. Catalog-page
cleanup, orphan/obligation completion and shared fees remain open.

[Finite old-root data cleanup](usage-date-data-reclamation.md) adds fixed 8 KiB
cursor/prefix-hash progress, bounded descriptor/page/file steps and aggregate
row/read/native/index budgets. Reopen replays the saved prefix and refuses a
claimed completed object that still exists; even saved Ready must reconstruct the
whole old descriptor chain. Exact-cap replay hands off another read-only chunk on
the same fresh owner so offline reopens can extend new work. Data Ready preserves
pages/obligation and does not supply physical fee refunds, orphan cleanup, native
construction resume or automatic generation rebase on normal appends.

The current fresh candidate adds fixed live counts, separate attempt/summary
removed-prefix positions, a minimum surviving floor and an atomic cleanup epoch.
[Finite window steps](usage-disk-window.md) delete eligible prefixes and their
scope indexes, withdraw an expired terminal version head before reclaiming its
observations, and retain one bounded pending request for later cleanup steps.
Attempt/summary/failure/detail continuations bind that epoch; public tokens are
version 2 with 196 characters. Existing fixed-H cursors remain usable on append,
and explicitly fail after deletion. Archive sources reject a start before their
independently recorded attempt cut.

The public [verified window entry point](usage-window-authority.md) uses a private
native-relative witness instead of a caller-supplied manifest frontier. A finite
publisher extends a matching witness only after actual publication and cleanup;
unacknowledged roots require their exact private receipt and repeated barriers.
Default Runtime window/retention guards remain, and no trusted saved-candidate
reopen, date retention or OS-space/RSS reclamation is claimed. Cumulative rollups,
quality/hourly/Dashboard state, active work and native accounting are preserved.

[Finite date-candidate comparison](usage-date-catalog-verification.md) now compares
bounded candidate files and native-derived sequence pages across calls, using a
private fixed-node disk index and untrusted checkpoint-prefix replay. It neither
activates a date root nor supplies missing native/role/Metadata publication
authority. Default Runtime and physical reclamation/fee boundaries remain open.

Explicit offline [orphan data-file reclamation](usage-date-orphan-file-reclamation.md)
can derive a bounded actual descriptor even after the original old catalog chain
is gone, then independently match native rows and exclude current references
before confined unlink/Sync/Close. This keeps the active policy receipt and pending
obligation, and does not provide directory inventory, policy succession or shared
physical-space refund authority.

[Finite live orphan data steps](usage-date-orphan-reclamation.md) retain at most
two confined roots/two directory streams and one pending name. The offline entry
continues read-only chunks on the same fresh owner before one finite unlink batch,
so ignored/protected prefixes do not perpetually restart at the first chunk. EOF
and whole-operation cleanup are checked, with no saved-offset or global namespace
completion trust. Default Runtime/HA and resource/production boundaries stay open.

## Orphan catalog metadata follow-up

[Finite orphan catalog page reclamation](usage-date-orphan-page-reclamation.md)
requires authenticated completion of the original old-page transitions, protects
both current and full candidate output chains, and only then inventories actual
metadata names in finite steps. It deletes no descriptor data files and leaves
the original obligation/policy pending. Observed EOF is not global orphan absence
or next-policy authority; see the contract and its affected local evidence.

## Persistent current-policy follow-up

[Persistent current date policy](usage-date-active-policy.md) can now preserve
one authentic historical original-chain result and its independently native-bound
policy in a bounded slot. Published verification/reclaim/retry can authenticate
the matching current policy even if the original marker is absent or holds a
future intent. This copy leaves orphans pending and preserves original files;
normal next-policy/transition succession and full obligation closure remain open.
