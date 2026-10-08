# Primary business Ledger disk handoff

Status: default Primary business Ledger disk selection implemented and locally
verified, 2026-10-04. One fresh business projection supplies Budget and Usage
evidence, and one real confirmed recovery continuation completes before
readiness/producer admission. Evidence:
[Primary disk runtime](../verification/performance/2026-10-04-primary-disk-runtime/README.md).
I-04/I-06 remain open for their remaining requirements.

## Startup order that the handoff must preserve

`Open` authenticates member state, reconstructs the ordering/native source,
installs Primary native hooks and returns Runtime before `Serve` starts peer
transport. Current peer adjudication then confirms the whole startup prefix and
publishes `startupReady`. Projected recovery's initial confirmation and each
subsequent recovery page need that actual transport. Performing the complete
projected Primary recovery synchronously inside Open would wait for a transport
that the caller cannot yet start. Removing its confirmation barrier is unsafe.

The default handoff implements this order:

1. Before native mutation/publication, validate all selected derivative paths
   against configured and computed authority/key/TLS paths and inode aliases.
2. Under the directory lock and the actual native maintenance lease, build a
   fresh disk business Ledger at the authenticated Primary durable native head.
   Revalidate current member identity/role/term/promise around construction;
   no saved derivative supplies facts, prefix permission or startup readiness.
3. Bind one projected Budget Manager under that lease. Prepare Usage without
   reacquiring the same native maintenance exclusion. Keep owner selection,
   lease ownership and cleanup explicit through failed Open and successful
   transfer to Runtime.
4. Return the nonready Runtime so the caller can start HA transport. Keep
   accounting mutation/background producers and ordinary business traffic
   withdrawn until recovery completes. Observability may remain available.
5. After peer adjudication and actual startup-prefix confirmation, run the
   real two-level recovery via `projectedRecoveryWriter` using the current
   Primary coordinator and lease. Do not synthesize confirmation or use the
   Standalone writer. Confirm every finite recovery batch and the complete
   final ordering target, catch Usage up and reconcile any affected archive
   window before enabling dependent producers/readiness.
6. Join the complete recovery algorithm before releasing its lease. Preserve
   native facts on cancellation/failure and keep business nonready. Readiness
   requires the same member identity/current promised term and a nonretired
   startup owner after the continuation finishes.
7. Withdraw startup publication on business drain/stepdown, cancel its work
   and join the actual continuation before retiring Budget/Usage/native/
   Metadata. A drain timeout may end the caller's wait but cannot authorize
   maintenance/promotion or close those dependencies. Terminal Close joins
   without treating cancellation as physical completion.

## Implemented prerequisite

The production `catchUpPrimaryPeer` now starts one owned completion worker
instead of an unjoined goroutine. It confirms the full startup target, checks
current member authority, optionally runs the prepublication-installed recovery
continuation and publishes readiness only afterward. Default Primary accounting
now installs `finishPrimaryAccountingStartup`: it uses the transferred native
lease and selected disk projection, runs real Primary two-level recovery, catches
Usage up, reapplies its window from untrimmed history and confirms the complete
final ordering target after any window/anchor Metadata repair. All ten ordinary
workers and the alert dispatcher wait for successful startup publication;
pressure observation remains available while startup is nonready.

Runtime Close, business drain and HA subsystem close withdraw the completion
owner. Permanent retirement and readiness publication share one admission lock;
no late completion can reopen readiness after retirement. The real completion
channel is reused by drain retries and actual Close. A coordinator whose
confirmation authority has been withdrawn cannot publish readiness even when
its already-confirmed target allows WaitConfirmed to return successfully.
The publisher, ordering
journal, coordinator and accounting dependencies outlive the worker.

Validation covers blocked continuation/Close dependency order, successful
completion, recovery failure, term promise during work, canceled retirement
without premature dependency closure and subsequent join of the same handle,
coordinator authority withdrawal, affected business-drain/Replica startup
regressions and real TLS HA transport.
Evidence: [startup completion lifetime](../verification/performance/2026-10-04-primary-startup-completion/README.md).

## Default selection evidence and remaining acceptance

Default Primary Open has no resident accounting State fallback. Real TLS tests
cover selected normal accounting append/Apply/ACK, accepted-before-lease
recovery, finalized-request pending attempts, late settlement replication,
role transition and empty structural roll. No-peer Open stays nonready,
rejects Usage maintenance and physically releases its transferred lease and
owned files during Close. NDJSON/Parquet retained Standalone seeds, next policy
and term withdrawal exercise the default shared business/Usage projection.
Lifecycle tests cover stale promise, failed continuation/coordinator withdrawal
and canceled retirement retaining the real join handle. Wider restart/migration,
crash fault matrices and production acceptance remain open.

Shared physical fees, unconfirmed append admission, trusted restart roots,
complete backup/migration and Linux/capacity acceptance remain separate open
requirements. The unconfirmed append boundary and required reservation handoff
are specified in the [Primary unconfirmed append admission contract](primary-unconfirmed-admission.md);
the current post-sync callbacks cannot safely enforce that admission. Fresh
full-native disk rebuild is still O(history) startup IO.
