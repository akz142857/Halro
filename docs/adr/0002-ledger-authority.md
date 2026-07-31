# ADR 0002: A single Ledger WAL is the accounting authority

- Status: Accepted
- Date: 2026-07-31

## Decision

Budget reservation, settlement, provider token usage, and provider cost use one physical Ledger WAL.

An `AttemptSettled` record atomically contains:

- reservation release;
- committed cost;
- provider input/output tokens;
- outcome and estimation flags.

The record is visible only after one complete framed append and fsync. There is no independent usage WAL.

## Commit order

Non-streaming:

```text
ReservationCreated durable
→ provider attempt
→ AttemptSettled durable
→ RequestFinalized durable
→ success response
```

Streaming:

```text
ReservationCreated durable
→ provider attempt
→ first downstream payload
→ AttemptSettled/RequestFinalized at stream end
```

An unclosed intent is conservatively restored as estimated committed usage. It is never silently refunded.

## Derived state

- bbolt checkpoint: acceleration only;
- in-memory aggregate: acceleration only;
- Parquet: historical analysis only.

Deleting any derived state must not alter the rebuilt ledger balance.
