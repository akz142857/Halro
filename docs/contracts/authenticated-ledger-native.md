# Authenticated Ledger native-source candidate

`internal/ledger/authenticated_projection.go` is an isolated D-03 maintenance
candidate. It is not selected by Runtime and has no saved-state reopen or native
resume authorization API. Default startup continues complete authenticated native
reconstruction. This contract does not grant readiness or HA promotion authority.

## Source and complete domain semantics

The caller holds exclusive, quiescent native maintenance ownership and supplies
an independently verified complete `ChainReport`. The builder creates a fresh,
exclusive component directory. Four canonical genesis constants initialize it;
all later domain rows come directly from `inspectReplayAuthenticated`. No
ordinary Bolt database participates as an input, index hint or trusted absence.

The original Ledger reducer is shared between Bolt and authenticated transaction
backends. A bounded dependency interface propagates read failures separately
from absence. Each native fact commits its balance, first event position, digest,
acceptance/finalization and legacy lease evidence, pending/time/count indexes,
attempt/request associations, settlements, work units, runs, governance
idempotency, list/export positions/counts and cohort versions in one transaction.
There is no separately visible index or position commit for that fact.

Epoch 1-3 history has CRC evidence, not native MAC content authentication. The
candidate rejects a source report with any checksum-only facts before creation;
falsifying that count fails the final actual-report comparison and discards the
private roots. A later MAC frame does not authenticate earlier CRC payloads.
An independently accepted legacy baseline remains part of the missing publisher
protocol. The existing ordinary native recovery path still supports that history;
this isolated component is not substituted into Runtime's compatibility path.

Repeated EventIDs retain their first physical position. Governance aliases keep
their own first position without inventing a second lifecycle effect. A finalized
request retains the original acceptance and unresolved attempt evidence, allowing
late settlement without restoring active request attribution or re-finalizing.
RunCreated enforces the same 32-Run lifetime limit using a per-WorkUnit scalar,
not by materializing all prior complete Run definitions. WorkUnitCreated seeds
zero; only a successful non-alias RunCreated increments it. RunClosed never
releases lifetime capacity. Count, definition, association index, digest and native
position share the same transaction. Missing, noncanonical or out-of-range
counts fail closed; a preexisting seed is never overwritten. Both backends build
these rows fresh from native facts, never from a saved untrusted counter.

## Codec, authenticated queries and limits

The stable scope binds an independently supplied Ledger identity, codec/reducer
version and complete 29-bucket catalog. Each tree key is a fixed bucket ordinal
followed by the original bounded projection key. Wide native Event/Request/
Attempt/Project identities use the existing SHA-256 lookup keys; original
identities survive in the complete authenticated values and association indexes.

The native cut digest includes scope, generation, offset, sequence, frame hash and
epoch. An HKDF purpose domain separates the component HMAC key from the supplied
32-byte native Ledger key. This is a candidate domain, not the production Vault
rotation, Metadata baseline or complete Seal protocol.

`Floor(target)` proves the greatest key <= target, including gaps between
subtrees, using authenticated ordered child bounds. A selected missing or altered
page/value is an integrity failure. The caller checks the returned bucket and
WorkUnit prefix; a preceding different scope never becomes a matching cohort
version. No historical version enumeration is needed for one latest-version
dependency. Floor shares Transaction's lifetime, cumulative read limits and
ignored-error abort behavior.

Before candidate creation or native source reads, the builder admits a separate
32MiB IO workspace in the same shared Manager. It keeps that claim until native
verification and all failure cleanup actually finish; Update's 64MiB workspace
is a separate simultaneous claim. Pressure denies work before opening source
files. These conservative logical estimates still require physical calibration;
business response ownership and all unrelated consumers remain Runtime work.

The shared reducer reads only the event's target WorkUnit/Run definitions and
writes definitions only for their applicable mutation kinds. In particular,
RunCreated neither reads all prior Run definitions nor rewrites an unchanged
WorkUnit. The per-WorkUnit counter is canonical JSON in the range 0..32 (one or
two bytes), addressed by a fixed two-hash meta key. Reducer scope is now
`codec1:apply2`, so this expanded row set cannot reuse an apply1 component scope.
The bucket catalog remains 29; the ordinary fresh Bolt backend has no reopen
migration path, and no native Ledger or Metadata schema is changed here.

All component limits from [the COW contract](authenticated-state-cow.md) still
apply: 64 mutations, 8MiB cumulative inputs/returned data, 256 read calls, 4096
objects and explicit append/pack ceilings. [Wide-definition verification](../verification/performance/2026-10-04-ledger-run-dependencies/README.md)
uses a 900KiB WorkUnit KeyID and 32 individual 900KiB Run KeyIDs in real MAC
native frames, checks the complete row set against fresh Bolt, and compares
lifecycle definitions with the full in-memory State oracle. It covers aliases,
physical repeats, independent WorkUnits, close without capacity release, Run 33
rejection, counter corruption and batch rollback. This proves this dependency
path; it does not establish a worst-case bound for every valid event kind.
Full native payload/dependency calibration and terminal/recovery capacity remain
requirements before selecting this backend.

## Prefix selection and publication barrier

An optional allowed prefix is copied at entry and matched against actual native
record or fully checked roll-boundary evidence. A canonical zero prefix selects
genesis. The excluded durable suffix is still completely authenticated. A wrong
hash, frame-interior cut, incomplete tail, changed source report, cancellation,
failed transaction or quota failure returns no usable candidate.

Each incremental component root is durable in the private unpublished directory.
The final verified physical head also covers an empty active/rolled generation.
The candidate becomes queryable only after the complete source report and exact
allowed boundary agree and the final position is committed. This local `ready`
flag is not global business readiness, cross-component publication or an HA cut.

Close joins owned physical readers/writer and retains persistent disk charges.
Discard uses the Store's captured-inode and writer-exclusion protocol, never
recursive deletion. Foreign output directories survive failed creation. A failed
or closed reader returns an error for both existing and invented event IDs;
it does not supply a successful empty balance or false absence.

## Evidence and remaining work

[Local verification](../verification/performance/2026-10-04-authenticated-ledger-native/README.md)
compares the complete row set in both directions with a fresh native-derived
Bolt reference. Its real native fixture includes compressed and rolled history,
149 facts, 29 buckets, 32 Runs, duplicate/alias events, finalization before late
settlement, unresolved request/attempt recovery and identities wider than Bolt's
key limit. A separate balance-only legacy reducer oracle preserves amounts and
duplicates while removing Run attribution uniformly, because legacy State retires
that attribution on finalization. It does not claim complete late-recovery oracle
coverage. Earlier Ledger semantic tests remain part of the affected package gate.

This removes ordinary source DB content from the candidate's initial trust path.
It does not close D-03 or I-11: independent production identity and typed roles,
the five-component Seal, Metadata epoch baseline/whole-file rotation, native
incremental Open, full business query interfaces, global integrity/readiness,
cross-process owner inheritance, GC, complete resource ownership and Linux
capacity/production acceptance are still unimplemented or unaccepted here.

## Live native advance

`AdvanceFromNative` operates only on the live native-built candidate. It binds
the source path and native key fingerprint retained by that builder; it offers
no arbitrary Record Apply or saved-store reopen capability. The caller establishes
the role-permitted physical prefix independently. A smaller prefix requires a
fresh reconstruction, never deletion of newer rows in the same root.

Entry uses zero waiting queues for the candidate and native mutexes, with a
separate 32MiB source IO claim in the same Manager. The native lock freezes append,
Roll, Compact and Close across both suffix passes. The first verifies the remaining
source and establishes the exact record/roll cut before changing any rows. Wrong
caller hashes do not skip excluded-suffix verification. The second commits only
allowed facts and verifies the complete remaining native tail again before
publishing query availability. Empty active/roll boundaries advance the physical
position with the same semantic sequence, hash and epoch.

Typed queries return Busy throughout the operation and cannot observe any private
intermediate root. Caller cut/identity/busy/pressure errors before reduction preserve
the old candidate. Native integrity faults or any failure once reduction begins
permanently retire it; cancellation/append quota cannot publish a partial result
or revive it on retry. Admission failures are not mislabeled as content corruption.
The Store can retain a durable private partial root after failure, but the Ledger
handle offers no way to serve or reopen it as an accepted checkpoint.

Owner/native Close signal cancellation and join actual advance work. Neither the
source charge nor physical reader/writer/key lifetimes retire merely on cancel.
Close still preserves persistent disk credit until captured-owner Discard finishes.
`AuthenticatedLedgerAdvance` returns no receipt on failure. Its From/To/NativeHead
and decoded/applied counts establish only this live component's barriers, not
global readiness, HA confirmation or an all-component Seal.

[Advance verification](../verification/performance/2026-10-04-authenticated-ledger-advance/README.md)
covers appends, compressed/empty rolls, allowed/denied cuts, mutation between
passes, quota/cancellation and actual Close joins. 50/500-fact active histories
with one new fact each decode two suffix facts across the two passes. These
counts exclude initial full native Open, directory lookup and physical reads
needed to seek a compressed offset. Older byte audits and complete existence/
length checks remain the enclosing protocol's responsibility; this method does
not establish final startup IO or physical capacity acceptance.


## Fixed-root recovery input

[Recovery input contract](authenticated-ledger-recovery.md) describes the live
candidate's callback-scoped pinned view. The independent pending/request streams,
request-attempt membership and lease dependencies all read one authenticated
input root while AdvanceFromNative may publish terminal facts into later roots.
This does not provide saved-store reopen or actual Runtime recovery selection;
Complete checks input enumeration and owner availability, not domain terminal
completion, HA confirmation or current role permission.
