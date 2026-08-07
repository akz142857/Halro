# ADR 0012: Deterministic price selection and cross-store consistency

- Status: Accepted; amended 2026-08-08 to make the pricing gate shared/exclusive,
  see "Amendment 2026-08-07"
- Date: 2026-08-04
- Tracking: GitHub Issue #76
- PRD: `docs/prd/prd-versioned-model-pricing.zh-CN.md`

## Context

Price versions are metadata in bbolt while attempt leases and price snapshots
are authoritative accounting facts in the Ledger WAL. A local transaction
cannot atomically commit both stores. Price selection also depends on time, and
wall-clock rollback, forward jumps, scheduled cancellation, or restoration of
an old backup can otherwise select a price timeline that has moved backwards.

## Decision

### Timeline and selection

Each deployment owns immutable `DeploymentPriceVersion` records and a timeline
index ordered by `(effective_from, version, id)`. The store enforces one version
per deployment and effective instant. Normal Admin APIs may create a current or
future successor but may not backdate or mutate an existing price term.

The Gateway captures one server-side UTC `pricing_selected_at` for admission
and attempt preparation. Selection is deterministic:

```text
highest effective_from <= pricing_selected_at
then highest version
then stable ID as a corruption-detection tie breaker
```

The final tie breaker must never be needed in healthy data; observing duplicate
timeline keys fails closed. Client-supplied time is ignored.

Each deployment persists a monotonic pricing high-water mark containing the
latest trusted selection time and latest observed effective instant. Small
clock skew within the configured tolerance clamps to the high-water mark.
Material rollback, unexplained forward jump, or an incoherent restored
high-water mark enters `pricing_quarantine`; the affected deployment cannot
serve traffic until reviewed.

### Deployment pricing gate and pin intent

The single-process implementation uses one in-memory pricing gate per
deployment — shared for selection, exclusive for timeline mutation, see "Lock
order and concurrency" — backed by a durable bbolt `price_pin_intent`:

1. under the gate, capture `pricing_selected_at`, select the version, and write
   a pin intent containing attempt ID, selected version/digest, metadata
   revision, and state `prepared`;
2. append and fsync the Accounting Lease with the self-contained snapshot;
3. in bbolt, mark the pin `committed` with the Ledger sequence;
4. leave the committed pin as navigational evidence until the attempt settles,
   after which it may be compacted under a durable retention rule.

Recovery resolves a `prepared` pin by looking for its deterministic attempt and
snapshot digest in the Ledger. If present, it completes the pin; if absent, it
deletes the uncommitted pin. Digest mismatch fails closed. The Ledger snapshot,
not the pin, remains the settlement authority.

Scheduled cancellation takes the same deployment gate and may succeed only
when `now < effective_from` and no prepared or committed pin references the
version. A Lease already durable before cancellation remains valid and settles
from its snapshot. A selection without a durable Lease may not start I/O.

### Mutation and Audit intent

Price creation, cancellation, restore confirmation, and proposal adoption use
a bbolt mutation intent containing the operation ID, actor, expected revision,
canonical request digest, target version, and Audit payload digest. The order is:

```text
bbolt mutation + pending audit intent commit
  -> Audit append/fsync
  -> bbolt delivered marker
```

Startup redelivers pending Audit intents idempotently. Audit may never claim a
successful mutation before the metadata transaction commits, and a committed
mutation may never permanently lose its Audit event.

### Lock order and concurrency

The global order is:

```text
deployment pricing gate -> bbolt pricing transaction -> Ledger project lock
```

Admin optimistic concurrency uses the Deployment/Price timeline revision and
stable `ErrRevisionConflict` semantics. This is a single-writer protocol, not
a distributed lock; multi-writer deployment requires a new ADR.

The deployment pricing gate is shared/exclusive. Gateway selection and pin
preparation take it **shared**; the Admin operations that mutate the timeline —
creation, scheduled cancellation, restore confirmation, proposal adoption — take
it **exclusive**. Selection and timeline mutation therefore still serialize
against each other, which is the invariant the gate exists for; two concurrent
selections do not, which the invariant never required. An exclusive acquirer
waits for every in-flight shared holder, so no cancellation can be interleaved
between a selection and its durable Lease, and `sync.RWMutex` blocks arriving
readers once a writer waits, so a steady stream of attempts cannot postpone a
cancellation indefinitely.

Because same-deployment selections now overlap, they can reach their durable pin
in the reverse of the order they captured `pricing_selected_at`. That is a
backwards step this process caused, not a clock rollback, so
`gateway.pricing_clock_rollback_tolerance` has a validated floor
(`config.MinPricingClockRollbackTolerance`) that must exceed the span between
capturing a selection time and committing its pin. Within the tolerance a
reordered selection clamps to the high-water mark; only a material rollback
quarantines.

See "Amendment 2026-08-07" for the measurements that motivated this and the
alternatives rejected along the way.

## Rejected alternatives

### Select from the current price during settlement

Rejected because historical cost would change with configuration state.

### Use only an in-memory mutex

Rejected because a crash between metadata selection and WAL append would leave
no evidence for recovery or scheduled cancellation.

### Put price records in the Ledger

Rejected because configuration lifecycle and queries belong in metadata, while
attempt snapshots already make the accounting record self-contained.

### Permit backdated normal price versions

Rejected because retroactive insertion changes which price an old request
should have selected. Historical correction uses adjustment events instead.

## Consequences

- bbolt gains price-version, timeline-index, pricing-high-water, pin-intent,
  and pricing-audit-intent records.
- Every selection has one authoritative time and canonical snapshot digest.
- Scheduled versions cannot be canceled while referenced by a durable attempt.
- Clock anomalies and suspect restores sacrifice availability for accounting
  correctness.
- The protocol is deliberately scoped to the current single-process writer.

## Required verification

- exact-boundary, before-boundary, after-boundary, overlap, and duplicate-key
  selection tests;
- property tests proving insertion order does not affect selection;
- concurrent selection/cancellation tests under the same deployment gate;
- kill points around pin-intent commit, Lease append/fsync, pin completion,
  metadata mutation, Audit append/fsync, and delivered marking;
- clock rollback/forward-jump and restore-high-water quarantine tests;
- deadlock tests enforcing the documented lock order;
- stable conflict, missing-price, and quarantine API error tests.

## Amendment 2026-08-07: the exclusive pricing gate is the Gateway throughput ceiling

Status: **Implemented 2026-08-08.** All three changes landed, and the protocol
sections above now describe what the code does. Two required-verification items
remain open and are marked as such at the end of this section.

### What was measured

Host: Apple M4 Pro, darwin/arm64, Go 1.24, APFS. On darwin, `os.File.Sync`
issues `F_FULLFSYNC`, so every durability figure here is a pessimistic bound
and no number in this section transfers to a Linux NVMe host.

The harnesses are committed, so the ceiling is a regression gate rather than a
one-off observation: `BenchmarkConcurrentAppend` (`internal/ledger`),
`BenchmarkMetadataWriteTransaction`, `BenchmarkMetadataBatchDelay`,
`BenchmarkDeploymentPricePinCeiling` (`internal/store/bolt`), and
`BenchmarkRequestLifecycle` (`internal/budget`). The figures below are the
re-measured values from those harnesses; they replace the original throwaway
numbers, which is why some differ from the first draft of this amendment.

Raw Ledger WAL append, group commit working as designed (`Options.MaxBatch`
defaults to 64, which caps the batch size):

| Concurrent appenders | events/s | mean events per batch |
|---:|---:|---:|
| 1 | 232 | 1.0 |
| 8 | 946 | 4.2 |
| 64 | 7,034 | 33.3 |
| 256 | 10,869 | 50.0 |

One bbolt write transaction (512-byte value, `FreelistMapType`), which is the
shape of both `PrepareDeploymentPricePin` and `CommitDeploymentPricePin`:

| Concurrent writers | `db.Update` tx/s | `db.Batch` tx/s |
|---:|---:|---:|
| 1 | 107 | 105 |
| 8 | 109 | 814 |
| 64 | 106 | 4,006 |

The Ledger scales with offered concurrency; `db.Update` does not, because it
never coalesces. `db.Batch` does, while staying at parity for an uncontended
write.

The durable pricing path a single deployment's attempts actually walk — shared
gate, prepare, commit — measured on the same harness with both sides built,
rather than reasoned about from the diff:

| Concurrent attempts | before (exclusive gate, `db.Update`) | after |
|---:|---:|---:|
| 1 | 55 attempts/s | 54 attempts/s |
| 8 | 52 attempts/s | 418 attempts/s |
| 64 | 56 attempts/s | 1,743 attempts/s |

Flat before, scaling after — the shape this amendment predicted. These absolute
numbers are higher than the ~32/s the ceiling formula gives because the harness
covers the two bbolt writes without the Ledger append between them.

### The finding

Stated in the present tense of the code as it was before this amendment landed.

`Service.startAttempt` held the deployment pricing gate across all three
durable steps of the protocol — pin prepare (`db.Update`), Accounting Lease
append and fsync, pin commit (a second `db.Update`). The gate was an exclusive
mutex, so attempts against one deployment could not overlap at all:

```text
per-deployment attempt ceiling ~= 1 / (2 * bbolt_fsync + 1 * wal_fsync)
```

On the reference host that was roughly 31 ms of exclusive critical section, or
about 32 attempts per second for a single deployment, and it did not improve
with added concurrency. Because the three steps were serialized by the gate, the
Ledger's group commit could not help either: same-deployment attempts were never
in flight simultaneously, so they never shared a WAL batch.

`Store.PrepareDeploymentPricePin` additionally held `pricingClockMu`, a single
process-wide mutex, across its entire `db.Update`. That made the prepare step
serialize across *all* deployments, not just within one.

This ceiling is a consequence of the concurrency protocol in this ADR, not of
an implementation defect. The ADR argues that selection and scheduled
cancellation must serialize against each other; it does not argue, anywhere,
that two concurrent *selections* on the same deployment must serialize against
each other. The exclusive mutex is stronger than the invariant requires.

One incidental property of the exclusive gate must be called out, because the
protocol leans on it implicitly: with selections serialized, the capture order
of `pricing_selected_at` equals the bbolt commit order per deployment, so the
high-water clamp path (`selectedAt < LatestSelectedAt`) is reached only on a
real wall-clock rollback. Concurrent selections give that property up — the
clamp becomes a routine path, not an anomaly path — and change 1 below must
keep it benign.

### Proposed change

1. **The deployment pricing gate becomes shared/exclusive.** Gateway selection
   and pin preparation take it shared; the Admin operations that mutate the
   timeline — create, scheduled cancellation, restore confirmation, proposal
   adoption, all four call sites in `internal/app/admin_prices.go` — take it
   exclusive. The lock order in "Lock order and concurrency" is otherwise
   unchanged.

   The cancellation invariant survives: an exclusive acquirer waits for every
   in-flight shared holder, so no cancellation can be interleaved between a
   selection and its durable Lease. What changes is that Admin price mutations
   may now wait behind in-flight attempts. That is acceptable — they are
   low-frequency operator actions, and the availability they trade away is
   bounded by one attempt's durable path.

   Shared holders commit out of order. Two concurrent prepares on the same
   deployment can commit in the reverse of their `pricing_selected_at` capture
   order, so the later commit observes a microsecond-scale "rollback" against
   the high-water mark. Under the default 2s rollback tolerance this clamps
   and is benign; with `pricing_clock_rollback_tolerance: 0` — which config
   validation permitted — it would quarantine the deployment on a reordering
   the gateway itself caused. Spurious quarantine from internal concurrency is
   a correctness bug, not a conservative default.

   **Resolved with a validated floor** (`config.MinPricingClockRollbackTolerance`,
   1s), not with clamp-only semantics. The alternative — stop quarantining on
   the durable high-water comparison and detect rollback solely from the
   in-memory clock anchor — was rejected because the anchor is empty after a
   restart, making the durable comparison the only detector that can catch a
   clock rolled back while the process was down. The floor must exceed the span
   a selection can spend between capturing its time and committing its pin;
   that span starts *after* the gate is acquired (`Service.startAttempt` takes
   the gate, then reads the clock), so a slow Admin mutation holding the gate
   exclusively cannot inflate it.

   Fixed alongside it: `Load` never merged `Default()`, so a config omitting
   `gateway.pricing_clock_forward_tolerance` decoded it as zero, which the store
   rejects on every priced attempt — a config that simply did not mention the
   key made the Gateway fail closed on all priced traffic. `Normalize` now fills
   both tolerances, as it already did for `gateway.source_rate_limit`.

2. **Pin prepare and commit use `db.Batch` instead of `db.Update`,** giving the
   metadata store the same group-commit property the Ledger already has. This
   only pays off once change 1 lets same-deployment attempts run concurrently;
   on its own it coalesces across deployments only. The failure-path
   `DeletePreparedDeploymentPricePin` deliberately stays `db.Update`: it is
   low-frequency and gains nothing from batching.

   Read against bbolt v1.5.0 (`db.go`, `func (db *DB) Batch` and `batch.run`),
   the retry contract is harsher than "may be invoked more than once" suggests,
   and imposes three preconditions:

   - **Any returned error rolls back the whole batch.** `batch.run` runs every
     queued fn inside one `db.Update`; the first fn to return an error aborts
     that transaction, is removed from the batch, is told to re-run solo, and
     **every remaining fn is then re-run from scratch** in a fresh transaction.
     A batch of *n* with *k* error-returning members costs up to *k+1*
     transactions and runs each surviving fn up to *k+1* times.
   - Therefore **expected outcomes must not be returned as errors.**
     `PrepareDeploymentPricePin` returns `ErrPriceUnavailable` on a deployment
     with no effective price — which `Service.startAttempt` treats as a *served*
     path under the unknown-price policy — and `ErrPricingQuarantined` on every
     request to a quarantined deployment, which clients keep retrying. Under
     `db.Batch` each such routine outcome would roll back and re-run an entire
     batch of unrelated deployments' prepares. Expected outcomes must be carried
     out in a captured result value with the fn returning `nil`, reserving fn
     errors for genuine faults.
   - Consequently **every captured outcome variable must be reset at fn entry.**
     Today `quarantined` is only ever set to `true` and never cleared, which is
     safe under `db.Update` but not under a re-run: a batch re-run executes in a
     *new* transaction, so it can observe committed state a non-batched writer
     produced in between, take a different branch, and inherit a stale outcome
     from the previous run.

   The transaction body is otherwise idempotent: all durable side effects go
   through `tx`, so a rollback discards them, and the captured
   `price`/`snapshot`/`intent` values are overwritten on each run. The
   `selectedAt` clamp mutates a captured variable but converges — a re-run sees
   the already-clamped value and takes the no-clamp branch to the same result.

   The first precondition also makes change 3 a **hard prerequisite rather than
   an optimization**: `db.Batch` only coalesces if multiple goroutines are
   inside it concurrently, so any lock held across the call reduces it to
   `db.Update` plus a `MaxBatchDelay` wait — strictly slower than today.

3. **`pricingClockMu` narrows to per-deployment — and its hold scope narrows
   too.** The per-deployment gate map (`Store.pricingGates`) already exists;
   the clock-observation map should be keyed the same way. But merely swapping
   the global mutex for a per-deployment one, held as today across the entire
   `db.Update`, would keep same-deployment prepares fully serialized — they
   could never share a `db.Batch`, and changes 1+2 would raise the
   single-deployment ceiling only to roughly one bbolt batch commit rate
   (~100/s on the reference host), not to Ledger group-commit territory.

   The required shape: the per-deployment clock lock covers only (a) the
   observation read and forward-jump inputs before the transaction and (b) a
   *monotonic merge* write-back after it — the map entry is overwritten only
   by a larger `SelectedAt`, because commit order may invert capture order.
   The forward-jump decision must be recomputed (or moved into the
   transaction) on any retry; a `db.Batch` solo re-run must not reuse a
   forward-jump verdict derived from a stale observation.

Changes 2 and 3 are safe under the existing protocol and could land first.
Change 1 amends the protocol and is the only one that raises the
single-deployment ceiling.

All three landed together on 2026-08-08, in the order 3 → 1 → 2, because change
2's benefit is unreachable until the other two let same-deployment prepares be in
flight at the same time. Change 1's rollback-tolerance floor landed with it.

### Rejected during this analysis

**Release the gate before the Ledger append.** This was considered and rejected:
it reopens exactly the window the gate exists to close, letting a scheduled
cancellation land between price selection and the durable Lease.

**Delete the bbolt pin and rely on the Ledger snapshot alone.** Rejected. The
pin is not a derivative of the Ledger. Recovery resolves a `prepared` pin by
searching the Ledger for its attempt and digest, and scheduled cancellation
needs the pin to know whether a durable attempt references a version. Removing
it re-creates the crash window that "Use only an in-memory mutex" was rejected
for.

### Out of scope for this ADR

A second, independent ceiling exists in `budget.Manager`: `lockProject` is held
across `appendApplyRecord`, so same-project requests cannot share a WAL batch.
That belongs to the accounting protocol, not to pricing, and has its own decision
record: `docs/adr/0018-project-admission-and-the-accounting-write-path.md`. Note
that its fix is *not* the one applied here — the project lock is exactly as strong
as its invariant, because two concurrent budget checks reading one balance really
must exclude each other. `BenchmarkRequestLifecycle` is committed here so that
record starts from a measurement rather than a remark — five Ledger events per
lifecycle:

| Projects | 1 worker | 8 workers | 64 workers |
|---:|---:|---:|---:|
| 1 | 47 lifecycles/s | 45 lifecycles/s | 45 lifecycles/s |
| 8 | 41 lifecycles/s | 177 lifecycles/s | 180 lifecycles/s |

The first row is the ceiling: flat in the worker count, exactly as the pricing
path was. The second row scales, and caps at roughly eight times the per-project
rate — confirming the bound is per-project, not global.

Because that ceiling (~45/s per project) nearly coincides with the pricing
ceiling this amendment removes (~54/s per deployment on the same harness),
traffic that reaches a deployment through a single project shows almost no
end-to-end improvement from this amendment alone: the bottleneck moves to
`lockProject` and the measured throughput barely changes. The gains here are
observable only under multi-project load, or after the budget decision record
lands. End-to-end verification must be designed with this in mind, or it will
falsely conclude the change is ineffective.

### Required verification for this amendment

Done:

- **Commit the benchmark harnesses** so the ceiling is a regression gate rather
  than a one-off observation — concurrent Ledger append with observed batch size,
  concurrent bbolt write transactions, and the five-event accounting lifecycle
  parameterised by worker count and project count. All three are listed under
  "What was measured", plus a before/after run of the single-deployment pricing
  path with both sides built.
- **Confirm `PrepareDeploymentPricePin`'s transaction body is idempotent under
  retry.** It is; the blocking issues were elsewhere, and are recorded as the
  three preconditions under change 2 — routine outcomes returned as errors, and
  captured outcome state not reset per run. Both are fixed.
- **Test the batch retry contract directly**, since it is now load-bearing:
  `TestPricePinPreparationSurvivesBatchSiblingFailures` runs deliberately failing
  siblings against concurrent prepares and asserts each caller's reported pin is
  the pin that committed.
- **Measure `MaxBatchDelay` against the host's real fsync cost and tune it.**
  `BenchmarkMetadataBatchDelay` sweeps it. bbolt's 10 ms default is the worst
  value on the curve; zero is no better, because the batch timer then fires
  before anyone can join. 250 µs is the chosen value — uncontended writes stay at
  parity with `db.Update`, eight concurrent writers reach ~7.5x it.
- **An exclusive Admin acquirer still excludes every in-flight shared selection,
  plus a starvation test.**
  `TestExclusivePricingGateWaitsForInFlightSelections` and
  `TestScheduledCancellationIsNotStarvedBySelections`. Go's `sync.RWMutex` blocks
  arriving readers while a writer waits, so the property comes from the standard
  library — the test pins it rather than implementing it.
- **Concurrent same-deployment prepares whose commit order inverts their capture
  order never quarantine**, at the smallest tolerance config accepts:
  `TestOutOfOrderSelectionsDoNotQuarantine` (scripted inversion, and a material
  rollback still quarantines) and
  `TestConcurrentSelectionsOnOneDeploymentDoNotQuarantine` (32 racing selections).
- **The narrowed clock lock's monotonic merge.** `TestClockAnchorDoesNotRegress`
  asserts the merge directly, and says why: an end-to-end pair cannot reach the
  case, because the durable clamp raises the later selection before the anchor is
  written, so a two-Prepare test passes whether the merge guards anything or not.
  `TestDurableHighWaterDoesNotRegressOnOutOfOrderSelections` covers the clamp
  itself. The forward-jump verdict is computed inside the transaction and the
  anchor is re-read per run, so no retry can decide from a stale observation.

Still open — neither is possible on this host:

- **Reproduce the measurements on Linux with an NVMe-backed data directory**,
  production build flags, and the race detector disabled. Every number in this
  section was taken on darwin/APFS, where `Sync` is `F_FULLFSYNC`; they are a
  shape, not a budget, and the *relative* before/after result is the load-bearing
  part.
- **End-to-end evidence, not just package benchmarks**: a deterministic local
  upstream and a load generator, reporting p50/p95/p99 and the first observed
  bottleneck, per `docs/verification/standalone-capacity-baseline.md`. The load
  matrix must include a multi-project-per-deployment shape; a single-project run
  cannot observe this amendment's effect at all (see "Out of scope").
