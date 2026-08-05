# ADR 0011: Durable accounting leases and crash recovery

- Status: Accepted
- Date: 2026-08-04
- Tracking: GitHub Issue #76
- PRD: `docs/prd/prd-versioned-model-pricing.zh-CN.md`

## Context

ADR 0002 makes the Ledger WAL the accounting authority. Its original
reservation model requires a positive monetary amount and therefore cannot
distinguish a metered request from a known-free request or an explicitly
allowed request whose price is unknown. It also does not durably distinguish
"prepared but never sent" from "possibly reached the Provider" after a crash.

Versioned pricing must preserve the exact price evidence selected before an
attempt and must settle from that evidence after a restart. Provider I/O cannot
be part of a local transaction, so the I/O boundary must be represented by a
durable Ledger fact.

## Decision

Every Provider attempt uses a tagged, durable `AccountingLease` written to the
single Ledger WAL before Provider I/O. The supported variants are:

- `metered`: a positive known reservation and a versioned metered price
  snapshot;
- `free`: a zero known reservation and a versioned free price snapshot;
- `unknown_allowed`: no monetary reservation, an unknown price snapshot, and
  durable policy evidence proving that cost governance was disabled and the
  instance explicitly allowed the request.

Zero and null are not interchangeable. Go models use tagged unions plus
pointers/options for nullable money. Validation, replay, balance calculation,
Usage, recovery, and export switch on the tag rather than on an integer value.

### Durable event protocol

For each attempt the authoritative order is:

```text
ReservationCreated(lease, PriceSnapshot, prepared token bounds)
  -> fsync
AttemptStarted
  -> fsync
Provider I/O
AttemptSettled(original PriceSnapshot, usage or conservative bounds)
  -> fsync
RequestFinalized
```

`AttemptStarted` is appended before any operation that can hand request bytes
to a Provider, including connection or transport behavior whose no-send status
cannot be proven. A transport failure is classified as "not started" only when
the adapter can prove that no request bytes could have reached the Provider.

The lease contains the attempt/request/project/period identity, deployment and
provider identity, lease mode, reservation, prepared input/output token bounds,
the full self-contained `PriceSnapshot`, pricing-selection time, policy
evidence for unknown pricing, and a recovery schema version. A Settlement must
match the lease identity, tag, formula, and price snapshot. Mismatch or integer
overflow is an invariant violation that makes accounting and readiness fail
closed.

### Recovery state machine

Startup replays the Ledger before opening listeners and resolves every pending
lease:

1. `ReservationCreated` without `AttemptStarted`: append a zero-cost
   `start_failed` Settlement and release the known reservation.
2. `ReservationCreated` plus `AttemptStarted` without Settlement: conservatively
   settle metered/free leases from the prepared token bounds and the original
   snapshot; settle `unknown_allowed` as unknown, never as zero.
3. Existing Settlement: perform no additional settlement.

Recovery event IDs are derived from `attempt_id`, the recovery action, and the
recovery schema version. The same event ID with the same payload is idempotent;
reuse with a different payload fails closed. Recovery appends and fsyncs the
Ledger event before advancing any bbolt checkpoint. A crash during recovery
therefore replays to the same result.

Pending leases with missing or contradictory snapshots, invalid tags,
overflowing calculations, or conflicting event IDs remain unresolved and keep
listeners closed. Operators receive stable error classes and aggregate
diagnostics, not price-source free text.

### Token Guard consistency

Admission captures one `pricing_selected_at` and a digest of the candidate
pricing view. Attempt creation rechecks that view under the deployment pricing
gate. If a later effective version raises the maximum cost, admission must be
re-evaluated; it may not reuse a smaller reservation. Prepared token bounds are
persisted in the lease and are the recovery ceiling.

### Locking and durability

The lock order is fixed as:

```text
deployment pricing gate -> bbolt pricing transaction -> Ledger project lock
```

No code may acquire these locks in reverse order. `free` and
`unknown_allowed` leases still participate in request concurrency, RPM/TPM,
Token Guard non-cost limits, and Provider attempt metrics.

## Rejected alternatives

### Treat zero as both free and unknown

Rejected because replay, reporting, and budgets could not distinguish a known
zero price from missing price evidence.

### Persist `AttemptStarted` after the Provider call begins

Rejected because a crash in the gap could incorrectly release an attempt that
reached the Provider.

### Store leases in bbolt and settlements in the WAL

Rejected because it creates two accounting authorities and an unrecoverable
cross-store commit window.

### Refund every pending lease at startup

Rejected because an already-started Provider request may have incurred cost.

## Consequences

- ADR 0002 remains in force: there is one accounting WAL.
- Known balances add metered reservations and committed known costs only;
  unknown attempts are tracked separately.
- More data is written before Provider I/O, increasing request-path fsync work
  in exchange for a provable crash boundary.
- Adapters and SafeTransport must expose a reviewed pre-I/O boundary.
- Usage checkpoints remain derived and disposable.

## Required verification

- tagged-union validation and round-trip tests for all lease/snapshot variants;
- formula overflow and component-rounding tests;
- same-ID/same-payload idempotency and same-ID/different-payload rejection;
- kill points before/after lease append, Started append, socket write,
  Settlement append, and during recovery Settlement;
- restart tests proving not-started release, started conservative settlement,
  unknown preservation, and no duplicate charge;
- Token Guard recheck tests across a price boundary;
- readiness tests for corrupt, contradictory, or unresolvable pending leases.
