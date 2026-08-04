# ADR 0013: Append-only cost adjustments and dual-time read models

- Status: Accepted
- Date: 2026-08-04
- Tracking: GitHub Issue #76
- PRD: `docs/prd-versioned-model-pricing.zh-CN.md`

## Context

Provider prices and invoices can arrive late or be corrected after an attempt
settles. Rewriting the original Settlement would destroy the evidence used for
the original budget and response decision. Inserting a backdated operational
price version would also change the apparent selection timeline.

Corrections must be auditable, idempotent, queryable by both service period and
posting period, and reflected in current-period budget state without making
historical closed periods consume today's budget.

## Decision

### Authoritative adjustment event

The single Ledger WAL gains append-only `CostAdjusted` events. An adjustment
references the original Settlement event ID and SHA-256 digest and contains:

- attempt/request/project/deployment/provider identity derived by the server;
- base Settlement cost, net cost before, signed delta, and net cost after;
- monotonically increasing per-attempt adjustment sequence;
- canonical idempotency-key digest and canonical request digest;
- service period/original completion time and posting period/posting time;
- reason code, bounded human reason, actor, and evidence digest;
- either a complete `CorrectionPriceSnapshot` for `reprice` or an explicit
  signed delta for an exceptional invoice difference.

The invariant is `after = before + delta`, and cumulative attempt cost cannot
be negative. The client supplies the attempt ID, mode, correction price or
delta, reason, and evidence; all identities, tokens, periods, original costs,
and calculated components come from authoritative replay state.

Within the Ledger project lock, creation atomically validates the expected
sequence/net cost, verifies the original Settlement digest, computes checked
integer results, enforces non-negative net cost, and appends the event. Same
idempotency key plus same canonical payload returns the original result; the
same key with different content fails closed.

An incorrect adjustment is corrected only with a subsequent reversing or
replacement event. Events cannot be edited, canceled, or deleted.

### Accounting and budget behavior

Project balance exposes separate original committed cost, adjustment delta,
and adjusted committed cost. For the currently open service period, positive
adjustments may create an over-budget state and block subsequent reservations;
they are never rejected merely because the correction exceeds budget. Current
period negative adjustments release capacity. Adjustments for a closed service
period do not consume or release today's admission budget.

Unknown original costs cannot accept an arithmetic delta until an operation
establishes a known correction snapshot or an explicitly reviewed invoice
amount. Known free attempts can be repriced through the same append-only path.

### Dual-time read models

Queries require an explicit `reporting_basis`:

- `service_period_restated`: attribute the delta to the original attempt's
  service period;
- `adjustment_posted`: attribute the delta to the actual posting time.

Dashboard cost trends default to `service_period_restated` and mark periods
changed by later adjustments. Operational reconciliation views use
`adjustment_posted`. No unlabeled aggregate may alternate between these bases.

Usage checkpoints are derivative and versioned. They retain Settlement by
attempt, cumulative delta, next sequence, idempotency digests, and both time
buckets. A checkpoint missing the adjustment schema is discarded and rebuilt
from the Ledger; missing fields are never interpreted as zero.

### Parquet and Audit

Existing Settlement partitions remain immutable. Adjustments are exported to
a separate append-only `cost_adjustments` dataset with its own watermark and
manifest. Query code reduces/joins both datasets by attempt ID. Sealed
partitions are not rewritten.

The Admin operation uses a durable bbolt adjustment intent before touching the
Ledger. The intent contains the deterministic adjustment event ID, canonical
request and idempotency digests, actor, expected sequence/net cost, and Audit
payload digest. The order is:

```text
bbolt pending adjustment intent commit
  -> Ledger CostAdjusted append/fsync under the project lock
  -> Audit append/fsync
  -> bbolt delivered marker
```

Startup checks the deterministic event ID in replay state. If the Ledger event
is absent, it revalidates the expected sequence/net cost and appends the same
event or fails closed on a conflict; if it is present, it verifies the payload
digest and resumes Audit delivery. Thus Ledger append remains authoritative
while no committed adjustment can permanently lose its Audit event. Audit
records use digests and stable identifiers rather than unrestricted evidence
content. Intent retention/compaction may occur only after the delivered marker
is durable and covered by a verified backup.

## Rejected alternatives

### Rewrite the original Settlement or Parquet row

Rejected because it erases the original decision and breaks replay and audit
evidence.

### Insert a backdated operational Price Version

Rejected because it changes the price timeline used to explain old attempts.

### Reject positive adjustments that exceed budget

Rejected because budget admission cannot override accounting truth.

### Store only the latest net cost

Rejected because individual corrections, evidence, sequences, and posting-time
operations would be lost.

## Consequences

- Ledger state and Usage aggregates gain signed checked arithmetic and
  per-attempt adjustment chains.
- Historical dashboards may restate earlier periods, but must visibly disclose
  that an adjustment caused the change.
- Exports include original Settlement rows and every adjustment row.
- Admin adjustment endpoints require recent re-authentication, authorization,
  optimistic concurrency, idempotency, and Audit.

## Required verification

- positive, negative, zero-net, and underflow/overflow adjustment tests;
- reprice component-rounding and original-token preservation tests;
- sequence conflict and idempotency collision tests;
- concurrent adjustments on the same attempt;
- current-period over-budget and closed-period non-admission behavior;
- equivalence of full replay, checkpoint replay, API aggregates, Dashboard,
  CSV, Settlement Parquet, and Adjustment Parquet for both reporting bases;
- kill points around Ledger append, Audit append, delivered marker, checkpoint,
  and both Parquet manifests.
