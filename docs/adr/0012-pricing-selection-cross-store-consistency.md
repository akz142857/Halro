# ADR 0012: Deterministic price selection and cross-store consistency

- Status: Accepted
- Date: 2026-08-04
- Tracking: GitHub Issue #76
- PRD: `docs/prd-versioned-model-pricing.zh-CN.md`

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
