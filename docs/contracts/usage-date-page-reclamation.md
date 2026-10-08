# Finite original catalog-page reclamation

Local implementation, 2026-10-03. `ReclaimRetainedDatePagesStep` reclaims the
original old catalog-page chain after genuine data processing. The offline
Standalone entry is `ReclaimUsageDatePagesStep`. The active root and its
[authenticated original policy](usage-date-policy-authority.md) remain intact;
old-root obligation completion, orphans and physical-space fee refund remain open.

## Completion witness and irreversible transition evidence

Deleting an old page destroys the ordinary untrusted cursor's replay input.
This operation therefore uses a distinct authenticated irreversible transition,
not a signed copy of the ordinary data checkpoint or its Ready field.

The data step proposes a fixed process witness only after it has reconstructed
all original descriptors, proved saved-prefix equality and checked actual absence
or current reference protection. The shared physical-job wrapper mints that witness
only after the projection job, target files and root/marker actual Closes succeed,
under the exporter writer/lifetime exclusion. Source/owner, current root bytes and
physical root/directory identity, original activation binding and completed prefix
digest are frozen. Failure, reset or actual owner Close withdraws it.

A missing `.archive-date-pages-v1.json` can be initialized only from this same
fresh owner's matching witness plus a new published-root comparison. Physical
reclamation additionally requires the current reference index. An unsigned saved
data Ready, a failed data job or a changed
physical root cannot bootstrap it. The canonical record is at most 8 KiB and
contains the original head, authenticated data-completion prefix digest/count,
next page, processed count, one pending next-hop intent and MAC. It contains no
history-sized deletion list, page array, index or descriptor array.

The MAC uses the independently derived native policy key under a separate page
transition domain. It binds the phase-normalized original signed activation,
therefore exact original roots/date/baseline and native-prefix hash. Every call
still independently authenticates that policy/native prefix, compares the current
root to fresh native content, and reconstructs current named references. This
does not authenticate a saved financial DB or approve the D-03 snapshot protocol.

## Before unlink and recovery

For a non-pending next page, the original content-addressed object must exist.
Its actual bounded bytes, hash, structure and predecessor/count relationship are
read and the actual file closed before signing an intent. The intent is file-
synced, actually closed, ownership-checked, renamed and directory-synced before
unlink. It records the verified predecessor so recovery can continue after that
page has disappeared. It never guesses a missing predecessor from filenames.

The physical operation checks current page-path membership separately from data
membership. It confines traversal to pinned actual parent handles, rejects
symlinks/nonregular/size mismatch, compares actual file and parent identities,
checks the expected checksum twice before unlink, and repeats current references
and source/root/marker fences. A currently referenced original page is preserved
and counted as protected. A referenced missing page is corruption, not success.
Every accepted path joins actual file/parent/temporary Close and directory Sync.

After a verified removal, protected skip or authenticated missing retry, a new
signed record advances to that predecessor. At most one checkpoint FD and one
pending page are retained between transitions. An interruption after unlink keeps
the preceding durable intent; a fresh owner repeats current authorization and
actual no-op Sync/Close, then advances without needing the vanished page bytes.
The whole report is withdrawn on any late error. Already committed unlinks or
signed records are not rolled back; the native facts and policy receipt remain.

Reopen accepts only a valid MAC and matching original policy binding. Forged
Ready/next hop, another content-addressed foreign page, modified page bytes,
wrong key, replaced parent/root, or a changed native branch cannot mint permission.
A normal authenticated append may change the fresh current root/source while
the original signed policy stays active: page progress keeps the original chain,
rebuilds the new current-reference index and protects newly referenced pages.
The ordinary comparison/reference checkpoints still require explicit reset when
their own frozen cut becomes stale; this is not general automatic rebase.

Deletion/loss or rollback of a signed transition cannot be replaced with an
untrusted guessed cursor. If its next non-pending page has already disappeared,
the operation fails closed. Backup/restore and explicit recovery of that evidence
remain part of the unfinished migration/fault matrix.

## Limits, reports and App boundary

Defaults are four pages, 16 MiB aggregate read IO, 16 MiB index IO and ten seconds.
Pages support 1–16; existing physical-job read/index/time bounds apply. Each input
page is at most 64 KiB; state updates are at most 8 KiB each, with one bootstrap
and at most two durable updates per processed page. The operation retains fixed
state, bounded page/stream buffers and confined traversal handles. The independent
native-policy resolver still scans complete history under the existing deadline;
its IO is separate from the finite page-read budget and is not resumable startup.

`ready` means the original authenticated page chain was processed, including
protected pages. It does not claim all unreferenced objects are absent now after
an external restore/recreation, finish the old-root obligation, or refund physical
disk fees. Logical bytes count only this call's actual unlinks. Missing retries,
protected pages and repeated Ready return zero logical deletion bytes.

An [explicit recreated/orphan data name](usage-date-orphan-file-reclamation.md)
can now be inspected without the vanished original descriptor chain. Its actual
bounded bytes generate an untrusted candidate descriptor; fresh native exact-row
comparison and current references still govern physical unlink. This does not
turn a historical signed page Ready into a current orphan-free directory proof.

[Finite live orphan data scanning](usage-date-orphan-reclamation.md) now visits
actual expired partition names and shares one batch's inspection/native/IO limits.
Unknown/outside-policy objects are preserved. Neither stream EOF nor a repeated
scan grants original-obligation completion or next-policy activation authority.

The App entry bootstraps at most one finite new data batch per call (read-only
prefix replay may use multiple finite chunks on the same owner), then at most one
finite page batch. A fresh signed-page resume does not replay a vanished old data
chain. The same exclusive directory lock, independently unlocked native source,
fresh projections, Metadata actual Close before mutation and final source/member/
Metadata/cleanup fences govern all reports. HA and persistent-member downgrade
are refused. CLI/default Runtime wiring, orphan reclamation, old-root obligation
completion and production acceptance are not enabled by this local API.

## Orphan catalog metadata follow-up

[Finite orphan catalog page reclamation](usage-date-orphan-page-reclamation.md)
requires authenticated completion of the original old-page transitions, protects
both current and full candidate output chains, and only then inventories actual
metadata names in finite steps. It deletes no descriptor data files and leaves
the original obligation/policy pending. Observed EOF is not global orphan absence
or next-policy authority; see the contract and its affected local evidence.

## Persistent current-policy follow-up

[Persistent current date policy](usage-date-active-policy.md) can now preserve
one authentic historical original-chain result and its independently native-bound
policy in a bounded slot. Published verification/reclaim/retry can authenticate
the matching current policy even if the original marker is absent or holds a
future intent. This copy leaves orphans pending and preserves original files;
normal next-policy/transition succession and full obligation closure remain open.
