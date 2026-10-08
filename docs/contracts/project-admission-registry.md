# Project admission identity and retirement

Budget Manager money decisions and WorkUnit/Run lifecycle operations share one
project admission identity. The selected State or disk Ledger remains the owner
of durable balances, leases, idempotency and lifecycle facts. This registry stores
only temporary admission/close coordination; it is never a persistent cache.

A caller retains identity before waiting on either money or lifecycle locks and
releases it after unlocking. The registry serializes lookup/refcount/removal, so
an admitted waiter cannot use an old lock while a new caller creates another for
that project. The registry mutex does not span a Ledger read, append or ACK.
The money mutex continues to end before durable append; global Apply ordering
and the existing conservative overlap of admitted and applied spend are unchanged.

Zero current users is insufficient for removal. Pending per-period money, Run
pending money, Run admission counts and closing markers retain identity between
calls. This includes zero-valued keys and Run admissions for zero/free/unknown
price amounts. Cond.Wait releases the money mutex but keeps its caller reference.
A Run-close marker lasts until its existing deferred cleanup, so later decisions
cannot bypass close while append/confirmation remains in progress.

After the last user exits and all four temporary maps are empty, removal releases
that project's entry. A fully empty registry drops its map reference too. Reusing
the ID later creates a fresh lock and reads the same selected durable owner; no
balance, reservation or lifecycle fact is expired. There is no TTL for unresolved
work. Manager retirement still clears references only after actual operation and
deferred cleanup join, before dependencies can close.

`halro_accounting_project_lock_live` reports the current registry entry count,
with no project labels or historical scan. It includes users/waiters and pending
obligations, and excludes cold/durable-only projects. It is not an accounting-cut
observation or a measurement of RAM. A nonempty registry can retain map capacity
from a past concurrency peak; obligation maps, lock waiters and their bytes also
need final shared resource admission/calibration. Entry removal alone does not
establish a hard memory ceiling for all active operations.

The default Manager and fresh disk Manager use this same registry. Native records
and reader formats are unchanged. This change does not publish a disk Runtime,
authorize role changes or make saved projection content trusted. Local race,
real native lifecycle/confirmation, metrics and static review evidence is at
[project registry verification](../verification/performance/2026-10-03-project-admission-registry/README.md).
