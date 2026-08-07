# ADR 0012: Deterministic price selection and cross-store consistency

- Status: Accepted; the concurrency protocol has a proposed amendment dated
  2026-08-07, see "Amendment 2026-08-07"
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
deployment, backed by a durable bbolt `price_pin_intent`:

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
stable `ErrRevisionConflict` semantics. Gateway selection and scheduled
cancellation serialize on the same gate. This is a single-writer protocol, not
a distributed lock; multi-writer deployment requires a new ADR.

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

Status: Proposed. Not implemented. The protocol above is what the code does today.

### What was measured

Host: Apple M4 Pro, darwin/arm64, Go 1.24, APFS. On darwin, `os.File.Sync`
issues `F_FULLFSYNC`, so every durability figure here is a pessimistic bound
and no number in this section transfers to a Linux NVMe host. The benchmarks
were written as throwaway harnesses and are not committed; reproducing them is
listed under "Required verification" below.

Raw Ledger WAL append, group commit working as designed:

| Concurrent appenders | events/s | mean events per batch |
|---:|---:|---:|
| 1 | 79 | 1.0 |
| 8 | 899 | 8.0 |
| 64 | 5,511 | 63.8 |
| 256 | 13,867 | 125.0 |

One bbolt write transaction (`db.Update`, 512-byte value, `FreelistMapType`),
which is the shape of both `PrepareDeploymentPricePin` and
`CommitDeploymentPricePin`:

| Concurrent writers | tx/s |
|---:|---:|
| 1 | 106 |
| 8 | 121 |
| 64 | 99 |

The Ledger scales with offered concurrency; bbolt does not, because the pin
writes use `db.Update` rather than `db.Batch` and therefore never coalesce.

### The finding

`Service.startAttempt` holds the deployment pricing gate across all three
durable steps of the protocol — pin prepare (`db.Update`), Accounting Lease
append and fsync, pin commit (a second `db.Update`). The gate is an exclusive
mutex, so attempts against one deployment cannot overlap at all:

```text
per-deployment attempt ceiling ~= 1 / (2 * bbolt_fsync + 1 * wal_fsync)
```

On the reference host that is roughly 31 ms of exclusive critical section, or
about 32 attempts per second for a single deployment, and it does not improve
with added concurrency. Because the three steps are serialized by the gate, the
Ledger's group commit cannot help either: same-deployment attempts are never in
flight simultaneously, so they never share a WAL batch.

`Store.PrepareDeploymentPricePin` additionally holds `pricingClockMu`, a single
process-wide mutex, across its entire `db.Update`. That makes the prepare step
serialize across *all* deployments, not just within one.

This ceiling is a consequence of the concurrency protocol in this ADR, not of
an implementation defect. The ADR argues that selection and scheduled
cancellation must serialize against each other; it does not argue, anywhere,
that two concurrent *selections* on the same deployment must serialize against
each other. The exclusive mutex is stronger than the invariant requires.

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

2. **Pin prepare and commit use `db.Batch` instead of `db.Update`,** giving the
   metadata store the same group-commit property the Ledger already has. This
   only pays off once change 1 lets same-deployment attempts run concurrently;
   on its own it coalesces across deployments only.

3. **`pricingClockMu` narrows to per-deployment.** The per-deployment gate map
   (`Store.pricingGates`) already exists; the clock-observation map should be
   keyed and locked the same way rather than behind one global mutex.

Changes 2 and 3 are safe under the existing protocol and could land first.
Change 1 amends the protocol and is the only one that raises the
single-deployment ceiling.

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
across `appendApplyRecord`, so same-project requests cannot share a WAL batch
and a single project measures about 30 request lifecycles per second on the
reference host regardless of concurrency (five Ledger events per lifecycle).
That belongs to the accounting protocol, not to pricing, and needs its own
decision record.

### Required verification for this amendment

- Reproduce the three measurements on Linux with an NVMe-backed data directory,
  production build flags, and the race detector disabled. The reference-host
  numbers above are a shape, not a budget.
- Commit the benchmark harnesses so the ceiling is a regression gate rather than
  a one-off observation: concurrent Ledger append with observed batch size,
  concurrent bbolt write transactions, and the full five-event accounting
  lifecycle parameterised by worker count and project count.
- Confirm `PrepareDeploymentPricePin`'s transaction body is idempotent under
  retry before adopting `db.Batch`. bbolt may invoke a batched function more
  than once, and the body advances a high-water revision and can set the
  quarantine flag.
- Measure the added latency of `db.Batch`'s `MaxBatchDelay` (10 ms by default)
  against the host's real fsync cost, and tune it rather than accepting the
  default.
- Extend the existing concurrent selection/cancellation tests to prove an
  exclusive Admin acquirer still excludes every in-flight shared selection, and
  add a starvation test so a steady stream of attempts cannot indefinitely
  postpone a scheduled cancellation.
- Add end-to-end evidence, not just package benchmarks: a deterministic local
  upstream and a load generator, reporting p50/p95/p99 and the first observed
  bottleneck, per `docs/verification/standalone-capacity-baseline.md`.
