# Authenticated Ledger fixed-root recovery input

`AuthenticatedLedgerProjection.WithRecoveryView` is a D-03 candidate input
adapter. It accepts only a live, native-built candidate; no saved DB, caller
resume anchor or unverified component root can enter through this API. Runtime
continues its existing fresh reconstruction and recovery selection.

The operation pins one authenticated Store root and reads the native position
from that same root. Pending attempts and unfinalized requests are separate
complete input sets at that position. A finalized request may retain a pending
attempt; a request may have no attempt or only settled attempts. Absence in one
set never supplies a proof about the other. RequestAttemptPage traverses the
complete authenticated association range and resolves lease/pending/settlement
point dependencies from the identical pin. ReadRequest also joins original
acceptance/legacy origin and finalization at that same head. No ordinary Bolt cursor or DB value
provides membership, absence or continuation evidence.

AdvanceFromNative can publish later roots between input pages. Terminal work
and new admissions cannot remove old input or join it midway through a scan.
The pin is input evidence, not permission to use a superseded role cut: the
owner/caller must still exclude business admission, establish current ordering
permission, write terminal facts and meet confirmation barriers. An advance failure that retires the candidate, a source integrity fault, Close
or cancellation of the whole operation withdraws further input reads and
Complete, even when the pinned pages would remain locally readable.

The caller receives a callback-scoped interface and must consume only bounded
pages/terminal work, join its own work and retain no response pages beyond that
scope. One callback is admitted per candidate with no waiting queue. Methods
also reject overlapping calls instead of queuing. Close from inside its own
callback is invalid; the lifecycle owner closes from outside that scope.

Each page has at most64 rows and8MiB source bytes, including dependent point
values. Enumeration uses authenticated one-row range continuations so an
additional wide dependency cannot advance a cursor beyond the returned data.
One lookahead is allowed; if a single record exceeds the byte allowance, the
whole page returns ErrProjectionPageLimit and leaves progress unchanged. A
smaller row count/larger legal byte allowance may retry. Source failures discard
the whole output page, never return a valid prefix or false absence.

Pending and request streams each keep one fixed next-cursor/Done slot. Cursors
are root/range MACs from the Store, not trusted numeric offsets; the two input
streams also require their actual next cursor, so they cannot skip or mix pages.
Request-attempt cursors bind the request's hashed scope and input root. No map
of pending IDs or request progress grows with history.

Complete requires both independent scans to reach Done and the owner/caller
still to be available. It does not prove that the caller walked every request's
attempts, wrote all terminal facts, completed recovery, or gained HA permission.
Ignoring a critical read error cannot complete the scope. A second final fence
after the callback returns rejects cancellation or withdrawal that occurred
after an earlier Complete. Escaped queries and Complete are permanently retired; Head remains an immutable
informational cut and grants no continuing owner availability.

A separate64MiB logical IO workspace covers retained bounded decoded/output
pages and one lookahead for the entire callback. Snapshot per-query32MiB and
pin bookkeeping remain separate charges in the same Manager. Callback cancel
alone does not refund any of those live owners. Close signals cancellation,
waits for the entire callback and its active query/cleanup to actually finish,
then closes the Store. Snapshot physical Close and workspace release precede
the join signal. Store persistent Disk and its claim bookkeeping survive Close;
captured-owner Discard removes the bytes before the final refund.

These estimates still need physical calibration. Deadline/root pin lifetime,
terminal/recovery reserve policy and all production consumer ownership remain
part of I-03/D-04 and Runtime integration. No unbounded callback or retained
page collection is licensed by this adapter. It is not a complete restart
checkpoint, cross-component Seal, Metadata/legacy baseline or business ready
proof. [Local evidence](../verification/performance/2026-10-04-authenticated-recovery-view/README.md)
records the actual native fixture, fault/lifetime cases and validation gates.

The [two-level recovery consumer](authenticated-ledger-two-level-recovery.md)
now combines original inputs with current authenticated settlements and the
App maintenance writer's full native ordering confirmation gates. Default
Runtime/backend and recovery-reserve selection remain separate work.
