# ADR 0018: Project admission and the accounting WAL write path

- Status: Proposed. Not implemented.
- Date: 2026-08-08
- Tracking: raised as out of scope by ADR 0012's "Amendment 2026-08-07"
- Related: `docs/adr/0002-ledger-authority.md`,
  `docs/adr/0011-accounting-lease-crash-recovery.md`,
  `docs/adr/0012-pricing-selection-cross-store-consistency.md`

## Context

`budget.Manager` holds a per-project mutex across the durable Ledger append. Five
events make up one request lifecycle — request accepted, reservation created,
attempt started, attempt settled, request finalized — and each one takes
`lockProject` and then waits inside `appendApplyRecord` for its own fsync. Same-project
events are therefore never in flight simultaneously, so they can never share a
WAL batch, and the per-project request rate is fixed at one lifecycle per five
serialized fsyncs.

`BenchmarkRequestLifecycle` (committed with ADR 0012's amendment) measures it on
the reference host — Apple M4 Pro, darwin/arm64, APFS, where `os.File.Sync`
issues `F_FULLFSYNC`, so these are pessimistic bounds that do not transfer to a
Linux NVMe host:

| Projects | 1 worker | 8 workers | 64 workers |
|---:|---:|---:|---:|
| 1 | 47 lifecycles/s | 45 lifecycles/s | 45 lifecycles/s |
| 8 | 41 lifecycles/s | 177 lifecycles/s | 180 lifecycles/s |

The first row is flat in the worker count: adding concurrency to one project buys
nothing. `BenchmarkConcurrentAppend` puts a single WAL appender at 232 events/s,
or 4.3 ms per append, and 5 × 4.3 ms = 21.5 ms → 46 lifecycles/s, which is the
measured number. The second row scales and caps near eight times the per-project
rate, confirming the bound is per-project rather than global.

This ceiling matters more than the pricing ceiling ADR 0012 removed, for two
reasons. Deployments are usually more numerous than projects, so per-project
bounds bind sooner. And unlike a per-deployment concurrency bound, this one is
independent of upstream latency: the five fsyncs are serialized against each other
regardless of how long the provider takes, so 45 requests per second per project
is a real request-rate ceiling rather than an artifact of how many requests are in
flight.

Two facts about the existing implementation decide the shape of the fix.

**The project lock is not what protects the read model.** `ledger.State` has its
own `sync.RWMutex`, and `State.Apply` refuses any record whose sequence is not
strictly after its watermark. `appendApplyRecord` additionally waits on a condition
variable until every earlier sequence has been applied. In-memory accounting state
therefore already advances in exact WAL sequence order, globally, without the
project lock's help.

**Four of the five call sites make no decision.** `BeginRequestDetailed`,
`MarkStarted`, `settle`, and `Finalize` take `lockProject`, build an event, and
append it. They read nothing they then act on. Only `reserveAttemptDetailed`
performs a read-modify-decide: it reads the project's balance, adds the proposed
reservation, compares the total against the daily budget, and appends only if the
budget allows.

That single decision is a genuine invariant, and it is why this cannot reuse ADR
0012's fix. There, the exclusive gate was stronger than the invariant required —
two concurrent price *selections* never needed to exclude each other. Here they do:
two concurrent reservations that both read the same balance and both pass the same
budget check will both be admitted, and the project ends the day over budget.
Making the lock shared would be a fail-open change to a spend control.

## Decision

### The admission decision keeps a lock; the durable append leaves it

Split `reserveAttemptDetailed` into an admission decision and a durable write. The
per-project lock covers only the decision, which is in-memory arithmetic and takes
microseconds. The append happens with no project lock held, so concurrent
same-project appends coalesce into shared WAL batches exactly as cross-project ones
already do.

Admitted-but-not-yet-durable spend is tracked in a `pendingAdmitted` map keyed by
`ledger.BalanceKey` — project, period, and timezone version together, not project
alone, because the daily budget is per period and a request near midnight belongs
to a different one.

```text
under the project lock:
    admitted = state.Balance(key) + pendingAdmitted[key]
    reject with ErrExceeded if admitted + reservation > dailyBudget
    pendingAdmitted[key] += reservation
release the lock

append + apply the reservation event        (no project lock held)

under the project lock:
    pendingAdmitted[key] -= reservation     (always, success or failure)
```

**Why this cannot over-admit.** The pending amount is added before the lock is
released and subtracted only after `Apply` has returned, and `Apply` is what makes
the reservation visible in `state.Balance`. So from the moment a reservation is
admitted until the moment it is durable and applied, it is counted in
`pendingAdmitted`; from `Apply` onward it is counted in `state.Balance`. There is
no interval in which it is counted in neither, which is the only interval that
could admit spend twice. The reverse overlap does exist — between `Apply` and the
subtraction a reservation is counted in both — and that direction is conservative:
it can only reject at the budget edge, never over-admit.

The subtraction runs on the same goroutine that added it, after the append returns,
so no bookkeeping is needed to decide whose amount to remove, and a failed append
rolls the amount back on exactly the same path as a successful one.

### The project lock is never held across a WAL append

This becomes a protocol rule of its own, not an incidental property of the
refactor. It is what keeps the ceiling from growing back the next time someone adds
a durable step, and it is also what keeps the lock order safe: the decision lock is
taken and released without ever nesting `ledger.State`'s mutex or `applyMu` inside
it.

### The four non-deciding call sites drop the lock entirely

`BeginRequestDetailed`, `MarkStarted`, `settle`, and `Finalize` append without
taking the project lock. Nothing is lost, because ordering comes from two other
places:

- **Global apply order** is already WAL sequence order, enforced by `State.Apply`'s
  watermark check and `appendApplyRecord`'s condition variable.
- **Per-attempt causality** comes from caller control flow. `State.Apply` requires a
  reservation to exist before `EventAttemptStarted` and before `EventAttemptSettled`,
  and `Service.startAttempt` awaits each call before making the next, so those
  events can never race. The requirement this places on callers — await each
  lifecycle step before the next for the same attempt — is made explicit in the
  package documentation rather than left implied by a lock that happened to be
  there.

Note that `EventReservationCreated` does *not* require its request-accepted event
to have been applied first, so even that pair is not ordering-critical to the state
machine; the usage read model creates its per-request accumulator on demand and is
order-tolerant too. Caller control flow keeps them ordered anyway.

## Rejected alternatives

### Make the project lock shared, as ADR 0012 did for the pricing gate

Rejected. The shapes look identical and are not. The pricing gate was stronger than
its invariant; this lock is exactly as strong as its invariant. Two concurrent
reservations reading one balance and both passing one budget check is
over-admission, and a spend control that fails open is worse than a slow one.

### Append the reservation first, then check the budget and compensate

Rejected. It costs a second fsync on the reject path, it makes an over-budget
reservation durable and therefore visible to `RecoverPendingLeases`, and it inverts
the rule that a reservation is durable *because* it was admitted. ADR 0002's
"reservation must be durable before Provider I/O" is not satisfied by "durable, and
we will decide about it shortly".

### Give each project a single writer goroutine and a queue

Rejected. It relocates the serialization without removing it: the queue's consumer
still performs one fsync per event for that project. It would help only if the
consumer batched several projects' events, which is precisely what the WAL's group
commit already does once the lock is off the fsync path.

### Approximate the budget in the hot path (token bucket, periodic reconciliation)

Rejected. Budget enforcement is one of the three properties this project puts ahead
of feature count, and an approximate check is an over-admission the operator cannot
see. The measured ceiling does not justify weakening the control when the exact
check can be made cheap instead.

### Key the admission lock by `BalanceKey` rather than by project

Considered and not adopted. It is the precise scope — admission decisions only
conflict within one balance key — but a project has one active period almost all
the time, so it would add a second keying surface for no measurable gain. The
pending amounts are keyed by `BalanceKey` regardless, which is where correctness
actually needs it.

### Reduce the five events per lifecycle

Out of scope here, and mostly not available. `EventAttemptStarted` looks
observational but is not: `State.Apply` uses it to mark the reservation `Started`,
which is what tells `RecoverPendingLeases` whether a crashed attempt may have
reached the provider and must be conservatively settled rather than released.
Collapsing it into the reservation would make every crash conservatively billed;
dropping it would refund attempts that were possibly billed upstream. Either is a
trade of accounting correctness for throughput and needs its own decision, not a
side effect of this one.

## Consequences

- Per-project request throughput stops being a function of fsync latency and
  becomes a function of WAL group-commit rate. On the reference host that is
  roughly 10,900 events/s at high concurrency, or about 2,100 lifecycles/s, versus
  45 today.
- **This is a throughput change, not a latency change.** One request still walks
  five causally sequential durable steps, so its accounting latency stays near
  5 × fsync (~21 ms on the reference host). Concurrent requests overlap; a single
  request does not get faster. Any capacity claim must say which of the two it
  means.
- A reservation may be transiently counted in both `pendingAdmitted` and
  `state.Balance`, so a project sitting exactly at its daily budget can see a
  spurious `ErrExceeded` that a serialized implementation would have admitted. This
  is accepted: it is the conservative direction, the window is microseconds, and the
  alternative overlap is over-admission.
- `pendingAdmitted` is in-memory only and bounded by in-flight attempts, which the
  per-project concurrency limiter already caps. A crash loses it, which is correct:
  admitted-but-not-appended spend never happened, and appended reservations are in
  the WAL where `RecoverPendingLeases` finds them.
- No durable format changes, so **no data-directory re-initialisation is required.**
- The terminal-apply behaviour is unchanged: an `Apply` failure still marks the
  manager unavailable and refuses subsequent appends, and the pending subtraction
  still runs on the way out.
- The single-writer, single-data-directory boundary is unchanged. This is a
  narrowing of an in-process lock, not a step toward multi-writer.

## Required verification

- A property test that concurrent reservations against a fixed daily budget never
  exceed it: many workers racing on one project, asserting that
  committed + reserved never exceeds the budget and that the excess requests get
  `ErrExceeded`. This is the fail-open direction and deserves the most scrutiny;
  run it with `-race` and `-count` above 1, since a single green run of a
  concurrency property is weak evidence.
- A test that a failed append rolls the pending amount back, so a project is not
  permanently charged headroom for a reservation that never became durable. The
  `ledger.Options.WrapDurability` seam already exists for injecting write and fsync
  faults.
- A test that admission is still correct while a settlement for the same project is
  appended but not yet applied — the case where `state.Balance` is deliberately
  stale in the conservative direction.
- A test pinning the new protocol rule: the project lock is not held across a WAL
  append. Prefer a structural check that fails loudly if a future durable step is
  added inside the critical section, over a timing test that only fails under load.
- `BenchmarkRequestLifecycle` at `projects=1` must scale with the worker count
  instead of staying flat. It is already committed, so the before number is on the
  record; report before and after from the same harness with both sides built,
  rather than reasoning from the diff.
- Confirm per-attempt causality survives without the lock by keeping the existing
  crash-recovery and lease-recovery tests green, and by asserting that
  `EventAttemptStarted` and `EventAttemptSettled` for one attempt are never applied
  before its reservation.
- Reproduce the throughput numbers on Linux with an NVMe-backed data directory,
  production build flags, and the race detector disabled. Every figure in this ADR
  is a darwin `F_FULLFSYNC` bound; the relative before/after result is the
  load-bearing part, not the absolute rate.
- End-to-end evidence per `docs/verification/standalone-capacity-baseline.md`,
  reporting p50/p95/p99 and the first observed bottleneck. With this ceiling
  removed, the next one is expected to move somewhere else — that ADR 0012's
  pricing path, the limiter, or the provider transport — and the baseline should
  say which.
