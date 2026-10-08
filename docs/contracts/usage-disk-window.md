# Bounded disk Usage window steps

Local implementation, 2026-10-03. This is a selected fresh owner's maintenance
primitive, not default Runtime selection, archive retention, native deletion,
trusted saved-DB recovery or production capacity acceptance.

## Source and deletion authority

The private `DiskProjection.pruneWindowStep` takes an explicit cutoff and exported sequence.
The caller must independently prove a complete archive publication for this
same selected owner. The method rejects a frontier beyond its Usage head; zero
frontier removes nothing. This check does not authenticate a manifest or authorize
a configured retention policy. In particular, `manifest.last_sequence` alone
cannot authorize the operation. The public [verified-window operation](usage-window-authority.md)
now requires a private native-relative witness for the same owner and root.
The finite publisher may extend that witness across a complete new prefix, or
create it from a missing root and actual source starting at zero. A failed
post-rename publication leaves only a private exact-root retry receipt; it cannot
authorize cleanup until repeated barriers and physical completion succeed.
Runtime window/retention guards remain until continuous publication authority
and date coverage are wired and verified.

The regression fixtures construct MAC-authenticated native Ledger facts, select
their actual Ledger owner, publish a real NDJSON archive, then perform exact
native-row verification before calling the primitive. Tests with an explicit
lagging frontier prove that newer facts remain; they do not claim a fabricated
frontier is an archive receipt.

## Finite transactions and positions

Limits are 1–128 logical rows and 1 byte–8 MiB input per domain. One call scans
at most twice these limits across attempts and summaries, including summary
version cleanup. Each logical row charges its encoded value and fixed index
companions; the exact terminal version and repeated cleanup reads are charged
again. Fixed metadata and bbolt page/freelist work are separate physical costs.
An individual row/companion that cannot fit an empty budget fails explicitly;
an exhausted batch after progress commits that progress for the next step.

Attempts and summaries advance independently, in native sequence order. Each
stops at its first record that is unexported or not expired; a later old timestamp
does not bypass that retained record. Main records, scope indexes, counters,
positions and cleanup epoch commit in one transaction. Input corruption, index
disagreement, counter underflow, epoch overflow and cancellation observed in
the transaction body roll back that transaction. Errors return a zero report. Cancellation/retirement
observed after a completed commit also withdraws the report; the committed
positions make a retry idempotent.

The fixed window state contains live counts, cumulative trimmed attempts, a
shared minimum-live `Floor`, and separate `AttemptAfter` / `SummaryAfter` cuts.
Floor uses the first surviving record of each domain, never the largest deletion
from either. An empty window uses one past the highest removed sequence, with
overflow refused. Attempts and summaries can therefore retain different cuts.
Internal archive streaming refuses an interval starting before `AttemptAfter`;
the shared floor cannot prove that attempt history still exists. Offline Verify
continues to build an independent fresh full-native reference.

## Request versions and continuation

Before dropping a terminal summary, the method checks its request/terminal
indexes and exact immutable terminal observation. It atomically withdraws that
summary's request-version head, so unreachable older observations cannot revive
detail. All observations for that expired terminal request are reclaimed through
the same bounded budget. If a batch ends mid-request, one constant-size pending
request hash and terminal sequence persists in the candidate DB. Subsequent steps
finish it before taking another main prefix; there is no historical work list.

Deleting any main row or immutable version advances a window epoch in the same
commit. Attempt, summary, failure-join and detail continuations capture that
epoch and refuse to continue after it changes, with no partial result. Append
does not change the epoch and existing fixed-H continuation semantics remain.
Public attempt/failure tokens use version 2, 196 raw URL-base64 characters; the
fixed payload and HMAC include the epoch. Old token versions are rejected. The
owner signing key remains ephemeral and independently random.

Maintenance uses zero-queue `TryLock` admission in the existing heavy-job then
physical-reader lock order. Active Summary, archive and complete-detail jobs
exclude it through their whole physical callback/cleanup lifetime; ordinary
page reads also exclude it. Cancelling a blocked callback does not allow an
overlapping cleanup before that callback actually exits. A step never launches
detached work or retains a read transaction between calls.

## Preserved and remaining state

Cumulative totals, metrics, daily rollups and their versions, quality/latency
indexes, Dashboard/hourly data, active/started work, the selected Ledger owner
and native facts remain unchanged by window deletion. A late settlement still
uses its original Ledger finalization/lease evidence, changes attempt accounting
once, and does not resurrect an expired or generate a second finalized summary.

The fresh projection format is version 6. There is still no trusted reopen API:
the pending cleanup supports steps within that physical candidate, not a claim
that saved derived bytes are authenticated after restart. A fresh Runtime rebuild
must reestablish policy/publication evidence and apply the window before exposing
its reconstructed detail; that publication path remains unfinished.

Default Runtime wiring and archive publication authority, date-based archive
coverage, catalog/data reader pins and unlink obligations, reclaiming other
historical indexes, shared physical fees, bbolt mmap/freelist/space, HA role
publication, authenticated incremental recovery and Linux acceptance remain
open. Logical row deletion is not an OS-space or RSS reclamation claim.

Evidence and source cut: [disk window verification](../verification/performance/2026-10-03-usage-disk-window/README.md).
