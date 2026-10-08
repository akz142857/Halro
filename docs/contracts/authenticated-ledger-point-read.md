# Live authenticated Ledger point reads

Status: component candidate, 2026-10-04. The only construction path is fresh canonical reduction of a completely authenticated native source. No saved root, ordinary DB, caller watermark, role or cross-component Seal is accepted as startup authority. Default Runtime readers are unchanged.

## Business surface

`AuthenticatedLedgerProjection` implements `AccountingLeaseReader` through `ReadLease` and `ReadPosition`. It also supplies balance, settlement, request, original finalization, legacy request shape, Run, WorkUnit, governance idempotency and first event position reads. The subsequent [accounting-read contract](authenticated-ledger-accounting-read.md) adds capacity, observations and coherent WorkUnit accounting. The later [admission-reader contract](authenticated-ledger-admission-reader.md) completes lists, maturity and cohort for this live candidate; it does not select a Runtime owner or authorize saved-root startup.

Each call pins one authenticated root. Required `meta/position` and all dependencies of that read use that same pin. Event position and its digest must both exist or both be absent; a missing half-index is corruption. Found leases, request origins/finalizations, domain rows and idempotency evidence must match the lookup identity. Physical sequences cannot exceed the pinned head. Settlements verify the event digest and known/unknown cost against the original fact. Legacy lease-first rows retain the real lease without inventing acceptance; finalization does not remove the origin needed by late settlement.

A missing business row is a verified absence only after the entire query succeeds. Missing required position, null/malformed JSON, invalid negative balances or identity/semantic conflicts reject output. Semantic or physical corruption retires this candidate for all point reads, Advance and existing/new recovery views. A correct old recovery pin cannot bypass owner retirement. Native Ledger health and facts are not changed by derivative semantic failure.

## Ownership and cancellation

Before opening the pin, one logical IO workspace of `AuthenticatedLedgerReadRAMBytes` (64 MiB) is acquired from the same Manager. It covers raw values, JSON decoding, validation and bounded output through the point-read job. Each Snapshot query separately acquires its 32 MiB query workspace, and the pin/claim bookkeeping is also charged. Advance's initial anchor read uses the same job while its source workspace is held. These are conservative logical floors, not RSS/cgroup acceptance.

No owner-lock waiter queue is admitted. Store pins and aggregate Manager quotas limit concurrent reads. Identifier input is limited to the native payload bound and row decode to the projection value bound. Caller retention of returned values needs the caller's own outer budget; returned objects are independent copies. These APIs are not a long-lived snapshot interface.

Owner Close cancels and joins reads, including actual synchronous decoding/validation and physical pin cleanup, before closing Store. Cancellation never refunds a still-running decoder. A final synchronous check covers caller cancellation, owner cancellation and global read failure before returning a value. Failure clears the typed result and found flag. Panic unwinds physical pin/fee cleanup and retires the candidate. Quota, pressure, caller cancellation and Busy refusal do not poison a healthy owner and can be retried after the condition changes.

## Actual consumer and remaining gates

The Metadata price-pin recovery API accepts the new reader under an independently frozen native maintenance lease. A local test uses 129 prepared pins (three Metadata pages), 65 real native leases, orphan removal, external lookup without a retained Metadata transaction, exact committed native sequences and an idempotent retry. This proves compatibility with that API, not default Runtime selection or safe incremental startup.

See [local evidence](../verification/performance/2026-10-04-authenticated-point-read/README.md). Saved-root Open, complete native incremental authentication, Metadata/legacy baselines, global Seal, actual Runtime two-level recovery/HA permissions, persistent disk/terminal credits, pin deadlines/GC and physical/production acceptance remain open.
