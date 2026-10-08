# Finite live orphan data inventory and reclamation

Local implementation, 2026-10-03. `ReclaimRetainedDateOrphansStep` extends
[explicit orphan-file verification](usage-date-orphan-file-reclamation.md) with
a finite live directory stream and one shared physical-job budget. The offline
Standalone entry is `ReclaimUsageDateOrphansStep`. This is data-file inventory;
orphan catalog pages, old-root obligation completion and safe policy succession
are separate unfinished work.

## Fixed live scan ownership

An Exporter retains at most one session with two confined `os.Root` handles and
two directory stream FDs: archive and current expired date partition. Entries
are read one at a time. The session retains one pending canonical path, fixed
counters and frozen source/policy/root identity. No directory/name array, date
map, old-chain list, persisted opaque offset or saved Ready is accepted.

Every step needs the same fresh projection generation, source head/window,
current root position/digest and physical directory identity, plus the original
phase-normalized signed activation binding. Each actual stream/root and named
partition identity is checked, with the physical-job source/root/marker fences.
A changed frozen source/root refuses the scan and closes it; it is not silently
rebased. Each new Exporter starts actual inventory from the beginning. A saved
directory offset could be used only as a future untrusted progress hint, never
as a namespace-completion or deletion receipt; no such offset is implemented.

`ResetRetainedDateOrphans` withdraws only the live scan and process proof and
actually closes its handles. It removes no filesystem object or checkpoint.
Exporter Close withdraws admission, joins active jobs and actual stream/index
Closes, then erases policy keys/proofs. Any accepted physical-job error discards
the scan before returning failure. Invalid arguments/Busy before admission do
not revoke another accepted live operation.

## Enumeration and candidate boundaries

The archive stream visits canonical `date=YYYY-MM-DD` partitions strictly older
than the active cutoff. Retained dates and non-partition root entries are ignored
and preserved. Invalid date-prefixed names, expired symlink/non-directory parents
or replaced pinned parents refuse the step. Inside an expired partition only
canonical visible `.ndjson`/`.parquet` leaves become candidates. Unknown/hidden/
nested objects are preserved and counted as unrecognized; no claim of their
content, ownership or physical deletion is made.

Current-root named membership is checked before decoding. A late current object
reusing an old date/path is preserved. An unreferenced candidate is described from
actual bounded bytes, then independently reverified against exact fresh native
rows before confined unlink/Sync/Close. The same original policy receipt remains
mandatory; an orphan filename or fresh checksum supplies no deletion authority.

Finding a row newer than the original prune H conservatively preserves that
object as outside policy. This is a preservation hint from actual decoded bytes,
not an assertion that the entire object matches native content. Only the precise
joined outside-policy error leaves are eligible for this skip. Actual file/spool/
parent cleanup, identity, IO, cancellation or corruption errors cannot be
suppressed because an outside-policy leaf also appears in the error tree.

The one pending name survives a successful entry/native-row boundary. It is
described and authorized again next call rather than inheriting an old descriptor
or permission. Missing retry has zero verified rows/logical bytes. Only actual
successful unlinks contribute logical bytes, which do not prove freed blocks or
permit shared disk-fee refunds. Any late failure withdraws the entire batch
report, while already committed unlinks remain and fresh scans can rediscover
remaining objects.

## Budgets and EOF meaning

Defaults: 128 directory entries, four candidate files, 8192 inspected rows and
4096 native-verified rows per step. Entries support 1–256, files 1–16 and
inspection rows from the per-file row limit through 2 million. The existing
file-size/read/native/index/time limits apply across every candidate in the
physical job, including both description and exact verification passes. Default
limits are 256 MiB/file, 1 GiB physical read IO, 64 MiB native values, 1 MiB index
IO and ten seconds. A conservative complete 8 KiB directory buffer is charged
before every directory read, including EOF. Inspected rows are debited before
processing each row and never refreshed between files. Mid-file exhaustion
fails closed; preceding unlinks are not rolled back. Native-row boundaries can
pause at a fully described pending name without exceeding verified-row limits.

Read-only finite chunks retain only the same-owner comparison proof. Any actual
unlink consumes it; the next physical batch needs fresh comparison authority.
`scan-complete` is delivered only after actual EOF and all live scan handles
close successfully. It describes that observed directory stream, not a proof of
a currently orphan-free filesystem. Deletion can change directory iteration and
external restores can recreate objects. Unknown/outside-policy objects remain;
another fresh scan starts from actual names. Every result keeps
`ObligationPending=true`. No signed global completion witness is minted here.

## Offline entry and remaining scope

The App continues finite read-only chunks on one fresh owner until a finite actual
unlink batch or stream EOF. This avoids permanently restarting a protected/ignored
prefix on every offline call. Read-only scan steps/entries/protected/outside-policy/
unknown/missing counts are reported separately from the last finite batch. After
actual deletion it stops; fresh calls restart inventory rather than trusting a
saved DB or directory stream. Fresh native construction and the total offline
operation still have the one-minute deadline and are not resumable initialization.

The existing exclusive data-directory lock, independent native keys, fresh
projections, readonly Metadata actual Close before mutation and final native/
Metadata/member/owner-cleanup fences govern reports. HA and persisted-member
configuration downgrade are refused. Arbitrarily large protected-prefix IO,
durable restart inventory, oversized legacy objects, orphan catalog pages,
old obligation completion/next-policy handoff, backup/rollback, shared RAM/disk
fees, CLI/default Runtime/HA wiring and Linux/production acceptance remain open.

## Orphan catalog metadata follow-up

[Finite orphan catalog page reclamation](usage-date-orphan-page-reclamation.md)
requires authenticated completion of the original old-page transitions, protects
both current and full candidate output chains, and only then inventories actual
metadata names in finite steps. It deletes no descriptor data files and leaves
the original obligation/policy pending. Observed EOF is not global orphan absence
or next-policy authority; see the contract and its affected local evidence.
