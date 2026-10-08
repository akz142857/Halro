# Standalone Usage startup owner

Status: default Standalone selection implemented, 2026-10-04. I-07 remains ongoing; affected integration validation is in progress.

`prepareStandaloneUsage` constructs the complete disk reducer and live Collector
under the caller's data-directory lock and Standalone native maintenance lease.
It requires the selected Ledger projection's physical position and chain hash
to equal the exclusive native head. A member configuration, missing lease or
different projection refuses publication.

The reserved `usage-projection.db` is disposable. Configured authority/key/TLS
paths and native hardlinks/symlinks are checked before removal. Normal Open also
reserves this name during its existing pre-mutation collision check. The
constructor discards old derivative bytes and streams the entire authenticated
native source into an exclusively created DB. It verifies the full replay's
head and the reducer's committed sequence/hash before returning a view. It does
not trust a prior Usage checkpoint, file checksum or saved derived database.

The reducer commits the native record's chain hash and epoch atomically with
its physical position, rows, indexes and counters. `ReadReplayAnchor` reads that
same fresh owner. The Collector authenticates a suffix from this cut, including
sealed EOF and generation transitions. There is no second in-memory progress
cache and no historical event-ID set. A failed/canceled call cannot move a
separate cursor ahead of a rolled-back transaction. This cut only has authority
because its inputs were authenticated in the current process; it does not
grant saved-DB restart or HA prefix authority.

Terminal Runtime close first retires the Collector and drains public query and
metrics lifetimes. It then joins the Usage derivative and physical DB before
closing the borrowed Ledger projection. The owned DB is removed only when the
current path still names the captured inode. A replacement file is preserved
and the identity failure is reported; unrelated store/lock cleanup still runs.
Failed construction joins actual DB work and returns no usable view/Collector.

The selected-disk public query/export fixtures now use this constructor and
Runtime cleanup instead of manually creating a disk view with a plain replay
callback. Tests cover real native facts, rebuild from garbage, cumulative
equality, incremental catch-up across an empty roll, live suffix corruption,
source-prefix mismatch, key/native aliases, replacement files and actual reader
join with the directory lock and Ledger dependency retained.

Selected Standalone disk window maintenance now runs one verified finite step
per tick (128 rows and 8 MiB independently for each prefix). It uses the live
console window setting and the Exporter's proof for this owner/root. Missing
proof, changed root, busy readers or errors preserve the rows. Member runtimes
cannot use this Standalone path. Main rows, indexes, versions, floor and epoch
change atomically; cumulative counters/rollup and archive bytes are preserved.
Repeated ticks drain the backlog. This is logical retention, not proof of
reclaimed filesystem allocation or a complete historical-index size bound.

Ordinary Standalone `Open` selects this fresh disk owner. It binds an independent
native-key exporter, observes recovery facts synchronously, then acquires a new
native maintenance lease and verifies/reapplies the stored window before starting
workers or publishing readiness. Recovery closes the earlier startup lease, so
that retired lease cannot authorize window restoration. Failed startup closes the
unpublished Usage/exporter, Governance, Ledger, Metadata and Vault dependencies.
Owner Close runs once, joins real work and preserves its first error.

The periodic tick uses the [recurring reference and policy controller](usage-periodic-reference-and-succession.md)
before archive append, then finite export and query-window trim. Public queries,
summary, dashboard, onboarding, metrics, Collector and archive use this selected
owner. There is no resident Aggregate fallback. Primary now uses a separate role-bound disk Usage owner with disk Ledger
evidence; its business accounting remains State. Replica retains its authenticated
applied-prefix legacy selection. See the current integration evidence for
Primary archive/term lifetime. Default Replica now selects its coherent applied-cut
disk Ledger/Usage generation. Default Primary now borrows its selected business
disk Ledger instead of owning a second evidence DB; confirmed recovery and
window restoration precede producer/readiness publication, as recorded in
[Primary handoff](primary-ledger-runtime-handoff.md).

[Current integration evidence](../verification/performance/2026-10-04-usage-default-runtime/README.md)
includes actual default Open, retained-date restart and both-format backup/restore;
full App and affected race results must be read from their recorded process state.
Shared RAM/disk fees, bbolt freelist/mmap calibration, bounded cold-start IO,
trusted incremental recovery, HA, Linux capacity and production acceptance also
remain open. See [the switching boundaries](../architecture/usage-retention-switch-boundaries.zh-CN.md)
and [local verification](../verification/performance/2026-10-04-usage-runtime-owner/README.md).
