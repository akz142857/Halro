# Persistent current date policy and original-chain completion

Local implementation. The [affected evidence](../verification/performance/2026-10-03-usage-date-active-policy/README.md)
separates local tests from CLI/default Runtime/HA/production acceptance.

## Two bounded records and independent source

The current date policy can now survive independently of its original enumeration
marker. `.archive-date-active-policy-v1.json` contains one original signed policy,
one authenticated historical original-page-chain result, a version and a distinct
completion HMAC. Its maximum is 136 KiB, independently of archive history length.
There is no prior-policy list, historical object array or saved global EOF.
The original 128 KiB activation record remains a separate slot.

`CompleteRetainedDateObligation` consumes a fresh same-owner published-root
comparison, current references, the original independently authenticated native
policy receipt and the signed old-page transition record. The transition must
match the exact original policy binding/old head, have no pending/next page and
record all original old pages processed. Its data-completion witness originally
came only from the successful actual data job and actual Closes; a saved unsigned
Ready or directory EOF cannot be converted into a completion signature.

The completion MAC uses `halro/usage/date-policy/active-completion/v1` and includes
the full policy/receipt and full authenticated original-chain result. The policy
continues to bind exact original old/new root bytes and native physical position
and chain hash. Every read still authenticates the original prefix from the
independently selected native source; valid completion/receipt MACs alone do not
establish saved-projection trust or permit native trim. Keys/resolvers retain their
existing actual operation/Close join and erase ordering.

The copied policy's derived phase is normalized to `published` only after the
fresh published-root proof and actual barriers. Replaying `intent` in the original
record cannot change the completion bytes or silently replace the signed decision.

## What completion means

`OriginalChainComplete=true` records authentic historical processing of the named
old chain. `PolicyPreserved=true` reports successful durable current-policy copy,
current/source checks and all actual cleanup. `AlreadyRecorded` is idempotent
publication of exactly the same canonical record.

`OrphansPending` always remains true. Restored objects, orphan data/catalog pages,
unknown objects and global directory absence remain separate physical work. The
operation deletes no current root, original activation/transition files, native
facts or data/catalog objects. It supplies no physical-space/shared-fee refund,
HA confirmation/promotion or whole-obligation/global orphan absence claim.
A different existing active record is refused rather than overwritten as a
convenient reset; normal next-policy handoff remains separate work.

## Physical publication and retry

The operation rechecks actual source/window/root/parent and original signed page
record identities and bytes. It uses bounded reads and verifies the page record
MAC/scope before constructing the completion. Publication creates a private temp,
writes/synchronizes and actually closes it, checks the name/inode, renames and
synchronizes the real directory. Original marker and current root barriers remain
inside the exclusive archive/source lifetime.

A fresh committed-record read verifies exact canonical bytes, completion MAC,
original receipt/native prefix and actual file identity; all record/source owners
Close before success escapes. Failure/cancellation, including late cleanup,
returns a zero report. A copy already committed before failure is retained and
can be authenticated/idempotently retried; no rollback is claimed. Actual Close
joins an unresponsive copy callback before clearing policy keys/resolver.

## Current-policy selection

Published verification, physical reclaim and published activation retry can read
at most two fixed records. Every present slot must parse, close correctly and
independently authenticate; malformed/unsigned/tampered records are not hidden
by falling back to another slot. Selection matches the current root's exact
retention, including its original prune H, and permits ordinary source/root
appends. Fresh full published comparison still proves the selected candidate
head is an ancestor of the actual current page chain and proves exact native row
coverage. A saved completion cannot replace that reconstruction.

The activation record takes precedence when it matches. If it contains an
authentic future intent with different retention, or is absent, the matching
retained active policy supplies current authority. Its exact full raw bytes are
bound in the process proof; physical reclaim re-hashes them at fences as well as
checking identity/size/mtime. Restoring mtime is not a content-change bypass.

A published retry using the active slot repeats actual file/root/directory barriers
without rewriting its completion MAC or marking another slot's future intent
published. A missing original marker over an existing retained root can no longer
be treated as first activation: an authentic completed active policy matching the
actual current retention must exist before signing a replacement intent.

The old activation is deliberately not automatically removed. Existing different
intents remain Busy; safe ordinary next-intent replacement, original transition
checkpoint succession, future completed-active replacement and interrupted/rollback
handoff recovery still need implementation. Two-slot reading is a prerequisite,
not a claim that full repeated policy maintenance is delivered.

## Offline App and remaining scope

`CompleteUsageDateObligation` uses the existing fresh native disk-reference factory,
exclusive directory ownership and offline Standalone publication wrapper. It
performs independent native/Metadata/member checks before and after mutation,
closes readonly Metadata before mutation and joins real owner/lock cleanup before
returning completion scope. HA and persistent-member configuration downgrade are
refused. Full native authentication/construction and the whole operation retain
the existing one-minute deadline, without resumable startup or Linux capacity
claims.

The [fixed-slot successor handoff](usage-periodic-reference-and-succession.md)
now binds the exact previous completion in the next intent MAC, requires a new
actual data-job witness to replace signed-page progress, and replaces the active
record only after the new original chain completes. Repeated policies and
rename-interrupted recovery are locally verified.

Remaining I-07/I-08 work includes recurring Runtime control and full obligation
closure, orphan inventory durable restart/large protected-prefix progress, signed
progress loss/rollback recovery, native initialization resume, shared physical
fees, CLI/default Usage/HA and Linux/production acceptance. I-07/I-08 remain ongoing.
