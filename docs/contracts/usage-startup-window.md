# Usage startup archive authority and window reapplication

Status: default Standalone startup integration implemented, 2026-10-04. I-07/I-08
remain ongoing. Ordinary Open runs this verification before readiness; this is
not a public partial-mode flag or authority to trust a saved derived database.

## Fresh owner and source binding

`restoreUsageDiskWindow` runs after a fresh reducer reaches recovered native
history and before publishing producers/readiness. Its maintenance lease must
belong to the exact selected native Log and have Standalone native hook mode.
Equal heads on a different Log, a member Runtime/configuration, another disk
alias or an already-trimmed query source cannot authorize this operation.

The selected Ledger's physical position/hash must match the frozen native cut.
The Usage replay anchor must match its sequence/hash. The archive is verified
against the untrimmed, freshly authenticated Usage source, not a saved rollup,
checkpoint, window floor or directory listing. A missing manifest leaves all
rows intact and returns `Verified=false`, with no trim step; a later publisher
can establish a new genesis proof.

Old JSON and paged complete-prefix archives use the exact full-row verifier.
Published retained-date roots use the independent native-key policy receipt,
original prune baseline, actual current-root ancestry and a fresh complete
date comparison. The startup caller explicitly discards only the prior
comparison hint, which cannot alter the policy, old-root obligation, catalog
pages or data. Saved Ready never becomes a shortcut across fresh owner builds.

The owner copies the independent native key during archive binding. Its prefix
resolver authenticates actual native bytes and checks their head/hash against
the selected native Log. Exporter Close joins real callbacks before that key
or native reference is cleared. Runtime terminal Close preserves the native
dependency through this archive join. This is independent policy authentication,
not a trusted derived-DB restart protocol.

## Published-date window permission

Only successful complete **published** comparison mints a window proof for that
fresh owner/current root. Candidate Ready, logical activation, a scoped/general
date report, saved policy MAC alone or an observed date/H does not mint it.
Every native attempt through archive H must either match a real archived row
or be an omission allowed by the independently authenticated date predicate
at/below its original prune baseline. This is not a new sequence floor or
permission to omit all old-date late arrivals.

The window step uses this same root/owner proof and the live console cutoff.
One call scans/deletes at most 128 rows and 8 MiB independently for attempts and
summaries/versions. Startup repeats steps to the first nonexpired/unexported
prefix boundary before returning its result. It preserves cumulative counters,
rollup, active requests, native facts and archive bytes. Clearing the query
window changes its cleanup epoch and invalidates old query continuations.

A matching native-relative publisher can extend the in-process permission
after actual publication and cleanup. The date predicate and original prune H
must remain identical; every new source row is published, including a late old
date above the original baseline. Unknown roots, another owner and reopening
inherit no permission. Failed publication retains only the existing private
exact-retry receipt and cannot authorize trimming until barriers succeed.

After trimming, the query DB is no longer a full-history reference. Its old
date proof cannot authorize physical reclamation; fresh date verification must
use an untrimmed native-derived reference. The window proof is scoped to derived
row retention and never authorizes archive/native unlink or Ledger compaction.

## Startup result and remaining integration

Before returning, the helper checks the selected owners again and authenticates
the actual complete native bytes once more. Every error/cancellation returns a
zero report. A late failure cannot undo an already committed derived trim, so
the startup caller must close/remove the unpublished owner rather than publish
it. On restart, a fresh build and fresh verification/reapplication reconstruct
the same cumulative facts and console window; unexported suffix rows survive.

Ordinary Standalone Open now selects the fresh disk owner and runs this helper.
The [recurring controller](usage-periodic-reference-and-succession.md) selects signed
current progress, completes old obligations and rebinds fresh window authority.
Full native cold-start/resolver IO is still O(history); finite individual steps
do not prove a bounded maintenance window. A canceled/failed Open publishes no
Runtime and releases all physical owners and the directory lock.
Shared physical fees, index/freelist/mmap capacity, trusted resume, HA, migration,
Linux capacity and production acceptance remain open.

See [local verification](../verification/performance/2026-10-04-usage-startup-window/README.md),
[startup owner](standalone-usage-runtime-owner.md) and
[switching boundaries](../architecture/usage-retention-switch-boundaries.zh-CN.md).
