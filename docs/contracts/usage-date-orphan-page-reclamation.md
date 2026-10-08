# Finite orphan catalog page reclamation

`Exporter.ReclaimRetainedDateOrphanPagesStep` reclaims detached catalog metadata
under fresh date-policy/native/current-root authority. It never follows a page's
data descriptors into data deletion. See the [data inventory](usage-date-orphan-reclamation.md),
[old-page transitions](usage-date-page-reclamation.md) and
[independent policy receipt](usage-date-policy-authority.md).

## Independent authorization and unfinished owners

Every admitted operation consumes the same owner's actual published-root
comparison and independently authorized original signed activation receipt.
It rechecks the frozen native watermark/window, current root bytes/inode, archive
directory, Metadata/caller role and current page-reference index. Ordinary saved
Ready/EOF/cursors, canonical names and page checksums grant no authority.

Before inventory, `.archive-date-pages-v1.json` must have a valid transition MAC,
match the exact original activation binding and old head, have no pending
transition/next page, and record all original old pages processed. Missing,
unsigned/forged Ready, wrong keys/bindings and authentic pending transitions are
refused. Thus an unreferenced page still needed to resume the original physical
cleanup cannot be mistaken for an orphan. This authentic original-chain evidence
does not complete the original obligation or prove an orphan-free filesystem.

An existing finite build checkpoint is an additional preservation hint:

- A candidate against the exact actual current root/digest, at the same or a
  later retain date, protects its entire output chain.
- The completed original build may remain after its old baseline disappeared.
  Its baseline/digest, completed input position, date and output must exactly
  match the original authenticated activation.
- Malformed or other stale/foreign builds refuse inventory. No automatic reset
  is performed to make deletion possible.
- An absent build has no resumable candidate ownership. Explicit build reset
  already discards that ownership while retaining its physical pages. An old
  comparison checkpoint cannot independently own/publish a candidate without
  its matching build. Current and originally activated pages remain protected
  by the fresh actual current-root index.

Unsigned build progress can conservatively protect pages; it cannot authorize
publication or deletion. A missing/corrupt candidate predecessor refuses the
operation before scanning. The original activation/new head is also proved an
ancestor of the fresh current root by the existing published comparison.
Synchronous export/migration writers share the Exporter mutex/lifetime exclusion;
there is no separate persisted unpublished catalog-head owner in those paths.

## Fixed working set and finite steps

The session owns one disposable private fixed-node disk radix index, two confined
roots, one directory stream FD, two parent identities, one candidate next ref,
fixed source/root/checkpoint digests and counters. It owns no page/name array,
history map, saved directory offset or durable EOF. The caller's current-reference
and comparison indexes remain separate; all three private indexes have fixed
RAM traversal bounds, independent disk caps and actual Close ownership.

Defaults per step are 128 returned directory names, four examined pages, four
candidate-chain pages, 16 MiB physical reads, 16 MiB index IO, a 4 GiB private
candidate index cap and ten seconds. The existing physical authority validates
read/index/time limits. Returned names and candidate/page work have explicit
smaller upper bounds (256/16/16). Each directory read, including EOF, is charged a
conservative full 8 KiB buffer. Checkpoint reads/re-hashes, metadata descriptions,
two unlink checksums and membership IO share the finite budgets.

All candidate output pages are read/structurally verified and indexed across
finite `protect-candidate` steps before directory enumeration. Each successful
read-only step may retain this process's fresh comparison permission; a real
unlink consumes it. A new owner rebuilds the candidate index and starts physical
inventory again. Native/root/policy/build/old-transition changes fail closed and
close the old session rather than rebasing it silently.

## Actual metadata operation

The inventory reads one name at a time in the actual confined `catalog/v1`
directory. It preserves unknown/noncanonical names, subdirectories and other
versions. A candidate must be exactly `page-<lowercase SHA256>.json`. Current and
candidate membership are checked before opening an unprotected object.

A regular nonempty object of at most 64 KiB is read completely. The filename hash
must match its actual bytes; version, bounded descriptor count/schema/paths and
predecessor reference shape must be valid. Page/descriptor counters are derived
from the full metadata object with checked arithmetic. Detached metadata may
refer to missing predecessors/data files: no traversal of those references or
claim about their native row content is made. Mixed retained/expired metadata is
eligible when its page itself has no live owner; all data files remain separate.

The actual unlink reopens that page under a pinned parent, excludes fresh current
references again, checks the observed actual inode/size/mtime, hashes twice,
invalidates old process cursors, removes only the leaf, synchronizes the real
parent directory and joins real file/parent Close. Every physical fence also
checks scan/index/parent ownership and held build/old-transition checkpoint
bytes/inodes. Checkpoint contents are re-hashed, so restoring mtime cannot hide a
change. Parent symlinks/replacements, content/inode replacement, IO/Close errors,
limits and cancellation withdraw the entire report. Already committed unlinks
are retained for a fresh physical retry, not represented as rolled back.

`LogicalBytesUnlinked` counts actual removed names' logical sizes only. Missing
objects count zero bytes. It grants no physical-block refund/shared-fee credit.

## EOF, reset and retirement

`scan-complete` is delivered only after actual directory EOF (or actual absent
catalog directories under empty reachable ownership), successful session Close
and scratch cleanup. A mutating directory stream can skip/repeat names, external
restore can reintroduce pages, and unknown names remain. Therefore every result
has `ObligationPending=true`; EOF is no global absence certificate.

`ResetRetainedDateOrphanPages` withdraws process permission and closes the actual
scan/root/index handles. It removes no catalog/data/current root/policy or signed
transition checkpoint. Accepted physical errors likewise discard the session;
pre-admission argument/Busy errors do not take an unrelated accepted job's owner.
Exporter retirement joins actual operations, then closes the scan/index and only
then clears the independent policy key/resolver. An unresponsive actual syscall
or callback cannot be shortened by cancellation.

## Offline App boundary and remaining work

`ReclaimUsageDateOrphanPagesStep` is an offline Standalone operation under the
existing directory lock and fresh native-derived disk projections. It keeps
finite candidate-protection/read-only chunks on one owner until observed EOF or
one finite actual unlink batch. Read-only counters are separate and checked for
overflow. It performs independent native/Metadata/member revalidation before and
after mutation and closes the readonly Metadata owner before mutation. Any late
source/cleanup/role/cancellation error returns a zero report. HA and persisted
member configuration downgrade are refused.

The original full native authentication/construction and whole offline operation
still use the existing one-minute deadline. Durable inventory restart, very
large protected-prefix progress, old-obligation completion/next-policy handoff,
persistent active-policy retention, signed-progress loss/rollback recovery,
shared physical fees, native initialization resume, CLI/default Runtime/HA and
Linux/production acceptance remain open. This API does not close I-07/I-08.

## Local evidence

The [affected local verification record](../verification/performance/2026-10-03-usage-date-orphan-pages/README.md)
separates focus/injection failures, final race/vet processes, actual source
fingerprints and production/HA/capacity limitations. Only terminal successful
processes constitute final gate evidence.
