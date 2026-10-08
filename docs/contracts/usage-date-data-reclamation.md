# Finite old-root data reclamation

Local implementation, 2026-10-03. `ReclaimRetainedDateStep` supplies finite scanning
and data-file removal for the one existing date-activation obligation. The offline
Standalone entry is `ReclaimUsageDateStep`. Each data removal uses the same actual
[file/native/parent/Sync/Close checks](usage-date-file-reclamation.md) as the explicit
single-file entry. Old catalog pages and the obligation remain pending.

The subsequent [finite page operation](usage-date-page-reclamation.md) can now
retire original pages from an actual successful data-job witness and independently
authenticated irreversible transitions. Ordinary saved data Ready cannot seed it;
after page cleanup begins, its signed resume replaces replay of the vanished old
data chain. The old-root obligation and active policy receipt still remain pending.

## Original-baseline authorization repair

A prior probe changed only the derived root and activation obligation, advanced
baseline 16 to 24, and wrongly removed two late archive rows while native WAL bytes
stayed unchanged. Both refusal assertions failed; the historical
[source/logs](../verification/performance/2026-10-03-usage-date-data-reclaim/README.md)
remain. Native content comparison alone did not authenticate the policy.

Both single-file and finite-step reclamation now require a
[native-bound authenticated policy receipt](usage-date-policy-authority.md).
It binds exact original roots/date/baseline and original physical native
position/hash. Reopen and physical operations check the signature and independently
resolved native prefix; unsigned legacy obligations cannot silently acquire
authority. The original probe and native-branch, key/signature, legacy and actual
Close regressions pass in the [local repair gates](../verification/performance/2026-10-03-usage-date-policy-authority/README.md).
CLI/default Runtime wiring and production acceptance remain incomplete.

## Frozen inputs and limits

The operation requires a fresh published-root comparison proof and process-owned
current-reference index from an independently authenticated native/role/Metadata
scope. It binds the actual current root/bytes/inode/directory, source head and
window, and phase-normalized activation obligation. The obligation's old root is
an untrusted enumeration hint, never deletion authority. Every old page still has
to pass actual ownership, structure and SHA checks, and every descriptor has to
pass canonical date/path and old-root coverage checks.

Defaults are 64 descriptors, four input pages, four data files, 4,096 verified
rows, 256 MiB per file, 1 GiB aggregate read IO, 64 MiB native point-read bytes,
64 MiB reference-index IO and ten seconds. Descriptors support 1–128, pages and
files each support 1–16; the existing file-limit ranges/codec bounds apply.
Read/native/index budgets and verified rows are shared across the entire step,
rather than reset for each file. A row budget stops before a next object that
cannot fit; an individual atomic file larger than the whole row/file/codec cap
is refused and still needs finite repacking. Low index/page/read budgets stop
between descriptors/pages; decoder/physical failures withdraw the whole report.

Eligibility is explicit date before C and maximum sequence at/below the original
prune baseline, followed by actual current-root reference exclusion. A referenced
old pathname is protected even if a late expired native append reused that name.
An eligible existing file must exactly match fresh native point reads, content
and real parent/inode ownership before unlink. Missing names receive fresh checks
and durable no-op handling, with zero rows/logical bytes. Native facts and fees
are never reclaimed by this operation.

## Fixed checkpoint and replay

One canonical `.archive-date-reclaim-v1.json`, at most 8 KiB, records frozen root
and obligation/source bindings, old page reference and offset, completed page and
descriptor/eligible counters, and a deterministic prefix hash. It never contains
a history-sized descriptor/deletion list. Only a fixed-size process replay state
is cached; it holds no old page, index object or file descriptor.

Reopen, changed checkpoint bytes, source owner generation or actual root/directory
identity recreates the process prefix state from the old head. Finite calls
recompute every saved descriptor/classification and the exact state at its saved
boundary. Each previously eligible unreferenced path must actually be missing;
an existing file, symlink or foreign replacement refuses the claimed processed
position without deleting it. A forged cursor/Ready cannot skip remaining files.
No saved Ready or prefix SHA is accepted as native authority.

Unfinished replay returns `revalidating`, no current-ready root, no persisted
trusted counters and no deletion. It may retain the original process comparison
proof solely across this read-only revalidation: every call repeats the source,
root, obligation, checkpoint/reference and actual Close checks. Any error,
cancel/retirement or real Close failure clears both proof and replay cache. The
first extension/checkpoint publication or completed replay consumes the proof.
The explicit single-file entry keeps its original one-admitted-call consumption.

If replay reaches the saved boundary exactly at an entry/page work cap while more
old descriptors remain, it still returns read-only `revalidating`. The next finite
chunk on the same fresh owner extends new work. Returning ordinary data progress
at that boundary would let offline reopens repeat the same prefix indefinitely.

After prefix equality is established, the step extends new finite data work and
saves progress only after each target's actual Close. The shared checkpoint
publisher writes, syncs, really closes, checks owner, renames and syncs the directory.
Its old checkpoint FD, actual source/root/obligation FDs and full job cleanup
must all succeed before progress is delivered. Before-save/after-rename failures
may follow committed unlink; the report becomes zero and fresh replay/missing
retry reestablishes progress. The obligation is never erased on failure.

`ready` means only that the entire old descriptor chain was processed. It retains
`ObligationPending=true`; old catalog pages and unknown orphans remain. Logical
unlink bytes do not prove physical block release or permit a shared fee refund.

[Finite old-page processing](usage-date-page-reclamation.md) and
[explicit orphan/recreated data inspection](usage-date-orphan-file-reclamation.md)
now provide distinct local follow-up operations. Neither completes the obligation
or proves all directory objects absent; orphan inventory and authenticated next
policy handoff remain unfinished.
`ResetRetainedDateReclamation` explicitly removes only the reserved checkpoint and
fixed replay cache, preserving data/pages/roots/obligation and withdrawing proof.

## Offline entry and remaining work

The App authenticates fresh native/Metadata references, constructs row comparison
and current references, and independently rechecks native/order/member/Metadata
before actual reclamation. Actual readonly Metadata Close precedes data work.
Read-only saved-prefix revalidation loops in finite chunks inside this one fresh
owner; then exactly one batch of new work is delivered. New offline calls reopen
and revalidate rather than trusting a saved DB/checkpoint. Configured HA and
persisted-member configuration downgrade are refused. Post-operation independent
source/Metadata checks and physical cleanup still govern success reports.

The complete App reference operation retains its one-minute deadline and full
fresh native construction. This is not resumable native initialization. Source
head/window or current-root changes refuse a stale frozen cleanup checkpoint;
automatic rebase while normal appends continue remains to be implemented. The
default Runtime/HA/CLI path is still open, along with page/orphan cleanup,
obligation completion, shared physical accounting, native construction resume,
Linux calibration and production acceptance. I-03/I-07/I-08 remain ongoing.
