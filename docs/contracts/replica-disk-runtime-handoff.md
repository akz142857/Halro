# Default Replica disk generation handoff

Status: default runtime integration implemented and locally verified,
2026-10-04. `openReplicaApplication` now selects one fresh
`replicaAccountingGeneration` owning disk Ledger and Usage. It installs that
generation as the actual ReplicaApplier target and borrows its public read
owners without building resident Ledger State or Usage Aggregate. Evidence:
[default Replica runtime gate](../verification/performance/2026-10-04-replica-default-runtime/README.md).
I-06/I-07 remain open; this gate does not grant production readiness.

Startup must retain the authenticated member publisher, ordering journal,
Metadata applied position and native source while constructing the fresh
generation. Its Ledger prefix comes from `AppliedIndex`, with
`Projection.Index == AppliedIndex <= ConfirmedIndex`; later durable or confirmed
native facts are authenticated but excluded from the initial projection. Usage
is rebuilt against that same disk Ledger. A saved DB, local checkpoint or
archive catalog cannot authorize a later prefix.

The existing candidate already supplies the live adapter needed by
`ReplicaApplier`: Ledger batches, structural rolls, Metadata apply, schema
validation and final native validation all hold a generation lifetime ticket.
Startup must install this same adapter and both selected read owners before
applying the confirmed suffix. Metadata and member identity/cut checks remain
mandatory around construction. Transport/readiness is published only after
actual `ApplyConfirmed` succeeds. Replica performs no semantic recovery, Usage
archive publication or native durable-tail checkpoint protection.

The generation owns its Ledger and Usage files exactly once. Runtime's public
read aliases must not become separate closing owners. Shutdown withdraws query
and metrics admission, joins background work and transport/applier calls, then
retires the generation before closing its native/Metadata dependencies. Failed
startup follows the same dependency order and returns no usable Runtime.

Default selection strengthens two candidate properties:

- Reserved output paths are checked against configured authority/key/TLS
  files, computed native paths, hardlinks and symlinks before any discard. Only
  disposable derived files can be rebuilt; excluded native tails remain intact.
- bbolt's actual exclusive OpenFile callback captures each created inode from
  the descriptor, without a create/close/reopen gap. Default Close joins admitted
  adapter calls, physically closes both DBs, preserves/reports replacement paths
  and retains the first physical cleanup error. Cancellation
  of a waiting caller does not establish physical completion or disk refund.

The integration proof must cover ordinary Replica Open with an applied prefix
behind the durable/confirmed tail, later actual confirmed apply, empty structural
roll followed by a semantic fact, finalized-request late settlement, corrupt
excluded native bytes, Metadata/cut mismatch, term change during construction,
protected-path aliases, replaced derived paths and a blocked adapter/Close join.
Ordinary Runtime tests now prove default selection, exclusion of an unconfirmed
durable semantic tail, actual CommitNotice/application into both disk readers,
startup application of an already-confirmed semantic suffix, persisted applied
prefix rebuild without duplicate counts, corrupt excluded-tail refusal and
cleanup. Construction/adapter tests cover the remaining cut/lifetime cases;
protected-path and inode replacement tests exercise the default factory and
actual exclusive file constructors. The real TLS Primary/Replica transport
test, including role handoff and structural roll, passes in an isolated race
run. An earlier combined run hit startup adjudication's deadline while indexes
were advancing; it remains recorded as a failure, and loaded startup timing
acceptance is still outstanding.

Shared physical RAM/disk claims, trusted saved-DB recovery, Linux growth/capacity
acceptance, restart/migration/backup compatibility and production acceptance
remain separate outstanding requirements. A fresh full-history disk build bounds
resident historical maps but still has O(history) cold-start IO.

Metadata application now commits each complete authenticated native frame and
its applied sequence atomically, under one serialized Store apply job. A failure
can leave a committed native prefix ahead of the unchanged member AppliedIndex;
that is resumable projection progress, never evidence to publish the failed whole
ApplyConfirmed. The caller still authorizes the confirmed target. Before the
first commit, the complete durable Log descriptor (including excluded tail) is
validated while holding its append lock; a same-position retry also validates.
Cancellation/Close stops subsequent frames and joins the actual operation. This
retains full-history scans, unpriced physical work and large atomic native ops;
it does not prove incremental startup or production capacity. Evidence:
[Replica Metadata recovery blocks](../verification/performance/2026-10-04-replica-metadata-blocks/README.md).

Metadata full validation now receives caller/Store cancellation through the
Context scanner, including its first validation pass and excluded durable tail;
normal Replica native Open receives startup's parent context. It checks between
bounded actual reads and bounded frame CPU work, waits for actual blocked Read
exit, and never publishes partial validation as success. Legacy Background APIs
and other journals' startup cancellation remain separate. Evidence:
[Metadata scan cancellation](../verification/performance/2026-10-04-metadata-scan-cancel/README.md).
