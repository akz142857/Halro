# Replica Ledger projection: permitted prefix and authenticated continuation

## Fresh build

`ledger.BuildLedgerProjectionPrefix` creates an exclusive new derived file. The
caller holds native maintenance ownership and supplies both the complete frozen
`ChainReport` and a role-permitted `ProjectionPrefix` (physical generation,
sequence, offset, chain hash). These values must come from authenticated native
and ordering/member evidence; an existing projection's metadata is not evidence.

The builder authenticates the complete frozen source, including the excluded
suffix, sealed generations and compression trailers. It reduces only facts in
the permitted physical prefix, using the existing 64-row/8MiB batches. A cut must
match an actual record or a fully checked offset-zero roll boundary; genesis is
explicitly all zero. Wrong hash, sequence, offset, generation, changed source,
partial tail, cancellation or a failed transaction discards the candidate.
Neither a later durable lease nor its balance/idempotency/active-request effects
may enter the rebuilt view.

A roll boundary can advance the physical cut without generating a semantic
event. The projection retains the last applied frame's epoch, so its
`ReadReplayAnchor` includes the anti-downgrade state with position and hash.
Checksum-era facts keep their existing checksum-only boundary; this API does not
turn them into authenticated facts.


## Constructing the exact startup cut

`InspectProjectionCursor` maps a caller-authorized generation/sequence to the
actual physical record or fully checked offset-zero roll boundary. Genesis and
initial generation/sequence-zero are distinct. It authenticates every native
file, including excluded bytes, using the stream inspector; wrong/missing cuts,
partial source, MAC/trailer/epoch failure and cancellation return no usable cut.
The lookup grants no role authority by itself.

`Log.BuildProjectionFromCursor` holds the keyed native lock across lookup and
fresh exclusive prefix construction. The inspection report must match the frozen
native head/hash/authenticated-chain state before construction. Both source passes
verify the complete source; second-pass source mismatch is checked before caller
cut mismatch so a valid same-key replacement cannot hide as a caller error.
Native read/verification/reader-close failure in either pass latches recovery;
output creation/DB and ordinary caller-cut/context failures do not supply native
corruption evidence. Failed candidates close/remove their newly created output.
A caller-owned existing output is never replaced or trusted.

`replicationRuntime.buildReplicaLedgerOwner` resolves only the authenticated
publisher AppliedIndex through a context-aware ordering scan. It compares member
identity, term/promise, all progress/MAC and Metadata projection position again
after construction, discarding the candidate on change. There is one ordering
prefix pass, two full native data passes and streaming manifest walks; memory
does not collect those histories. Native syscalls/lock waits remain cooperative
cancellation and can delay join.

This factory prepares a candidate; it does not swap default Runtime owners,
publish readiness or retire another generation. The caller holds startup or
maintenance lifetime ownership, admits RAM/disk/IO costs, and must revalidate the
role cut at the final coherent reader/reducer/Usage handoff. A returned candidate
cannot be treated as an enduring role authorization token.

## Same-owner continuation

`replication.NewNativeProjectionWithLedgerOwner` binds one selected Ledger read
and reducer owner; it does not retain a second historical State. The disk owner
supplies an errorful position and `ReplayAnchor`. Shared Metadata and derivative
callbacks belong to the caller. The adapter serializes Ledger continuation, and
its caller must join all applier operations before closing or reclaiming owners.
It has no independent retirement or resource-budget claim.

`ledger.Log.ReplayAuthenticated` freezes native append/roll/close with the native
lock. It continues from the trusted anchor's position, hash and epoch, checking
MACs in the active file and at nonzero sealed offsets, roll chain links, complete
sealed EOF/trailers and the final frozen native position/hash/epoch. Authenticated
Records passed to the reducer carry the actual chain hash. The older
`Log.Replay` cannot provide this contract for active/nonzero continuation.
The legacy State adapter now derives one native authenticated anchor from its
fresh role-permitted startup position, and updates it after every successful
Apply/roll. State itself still stores no hash; this does not authenticate saved
State contents or authorize a role prefix across restart. External position
changes are refused.

`ReplicaApplier.ApplyConfirmed` selects the allowed ordering prefix. It applies
Metadata first, then makes a second bounded ordering pass for Ledger batches and
structural rolls, without collecting the suffix. Intermediate batches use
`ReplayAuthenticatedGeneration` to validate the complete target generation,
including sealed EOF/trailer, end hash and epoch declaration; they do not scan
later generations repeatedly. A final `ValidateLedgerForPublication` authenticates
the remaining native suffix once, without reducing excluded facts or invoking
observers. Generation lookup is binary over the existing resident sorted directory. Derived callbacks execute before the selected owner and must be
idempotent: after a later callback/transaction failure they can see the same
record again. Receiver `applied_index` advances only after all required local
projection/validation work succeeds. Local `ledgerDone` retains only the last
successful Ledger substep index in the current applier lifetime. It grants no
member publication authority; the final owner/anchor/native-health/suffix barrier
still runs when retries skip every previously successful Ledger substep.

Native integrity or native reader-close failure latches recovery-required. An
owner may already contain the target record when a later native suffix/trailer
fails; this does not authorize applied-index advancement. Same-position retries
must refuse the damaged generation. Consumer errors and cancellation are
retryable, but the adapter retains `needsValidation`; even if the owner reached
the target, retry must finish the remaining native validation before succeeding.
A closed, absent or unreadable disk owner never falls back to State. Regressed
sequence/generation and same-sequence generation mismatches are rejected.

## Confirmed physical roll

`Log.WithVerifiedProjectionRoll` freezes native mutation/close, checks the ordered
plain segment identity against its locally published representation and authenticates
its complete bytes, EOF/trailer and epoch. Compression/file representation can
differ between members. Its private proof permits `ApplyVerifiedRoll` to move
only from the exact sealed head/hash/epoch to the next generation at offset zero,
or idempotently retain that exact successor. The disk owner changes only position
in one transaction; balances, events, requests, leases and maturity are untouched.
No semantic event is fabricated. Missing semantic work or a forged proof is refused.
The caller separately establishes that the roll belongs to the confirmed prefix.

Native lock ownership spans the selected-owner callback. Cancellation or consumer
failure after an owner commit cannot authorize whole AppliedIndex publication;
retry keeps the physical cut and completes validation before publishing. Native
Close waits for that callback, and callers must continue respecting native-to-owner
lock order and avoid callbacks into the native Log.

## Usage with the selected owner

`usage.LedgerDerivative` binds the Aggregate's event/finalization reader to the
selected Ledger owner before fresh construction or checkpoint restore. All live
Usage writes must go through the derivative. The existing callback-before-Ledger-
Apply order is preserved: persistent event position wins, IO fails directly, and
only an absent immediate next native record can borrow its current EventID and
physical position until Apply returns. Historical absence, physical discontinuity
or owner error cannot use that fallback. Structural rolls must move the owner
before the next generation's record. Native authentication and role permission
remain caller responsibilities; the bridge creates neither.

Finalization always reads the persistent owner. Late settlement preserves the
terminal request summary and original attribution. A later derivative failure can
leave Usage ahead of Ledger locally; retry must remain idempotent and cannot
publish member applied_index until the entire projection/native barrier passes.

Close withdraws future Apply, joins admitted Apply and reader IO, then breaks
owner/aggregate references. Old Aggregate reader aliases retain the retired
wrapper and fail; callers must release their Aggregate aliases and close their
own DB after join. Close/owner callbacks cannot re-enter Close or Aggregate().
There is one borrowed identity/position and no new history map or queue; the
underlying Aggregate history and RAM/disk fees still require I-07 and I-03.

Local evidence: [selected owner/live Usage bridge](../verification/performance/2026-10-03-replica-usage-bridge/README.md).

## Transport lifetime before owner retirement

`ConnectionManager.Close` permanently withdraws serving, cancels the manager's
serving context, and joins all admitted handshakes, sessions and callbacks before
clearing its key. Data registry lease drain and the outer Serve worker join cover
registered preparation, authenticated Hello and control-only callbacks as well.
Concurrent Close calls await one completion; Close before Serve permanently
rejects serving. A manager-owned callback must not call this joining Close.
Individual session Close continues to avoid joining borrowed handlers.

Terminal Runtime Accounting close stops confirmation waiters, then joins the
transport before closing native Ledger or retiring Accounting. Replication
foundation close similarly joins before closing publisher/source/journal or
clearing the cluster key. Normal business drain keeps confirmation available.
This barrier does not publish or select a replacement role generation. Native
syscalls and callbacks ignoring cancellation may delay the join; dependencies
remain alive until they actually exit.

Local evidence: [transport and terminal-owner join](../verification/performance/2026-10-03-replication-transport-join/README.md).

## Replica accounting candidate preparation

`prepareReplicaAccountingGeneration` prepares one fresh disk Ledger, a fresh disk Usage
bridge and an adapter for an exclusive startup/maintenance caller. It checks actual
Metadata applied epoch/sequence and transaction ID against member projection before
and after construction, and rechecks the complete role/member cut. Any change or
failure discards the owned candidate; shared native and Metadata are preserved.

Full native authentication covers the durable history while Usage consumes only
the role-permitted semantic prefix. A structural roll can place Ledger at the next
generation's offset zero while Usage retains the preceding last event position.
Continuation then consumes the next confirmed native frame through the same owner.
Saved Usage checkpoints/rollups are neither trusted nor reset. Before selecting this
fresh candidate, coherent publication must replace/stage persistent derived Usage;
flushing fresh deltas onto old rollups could double-count. D-03 authorization and
I-07 full bounded queries/retention remain separate unfinished work. The candidate
now keeps Usage history in its own exclusive derived DB; mutable stats are current
short-View observations and immutable detail cursors freeze their permitted head.

The composite ticket joins entire adapter calls before retiring Usage and Ledger
and deleting only the owned output. Callers own shared Metadata/native lifetime,
external applier/business drain, admission fees and final publication revalidation.
`ReadMetadataAppliedPosition` provides a short errorful read, not content trust or
permission for concurrent Store.Close. This factory does not select defaults or
publish readiness.

Local evidence: [Replica accounting candidate](../verification/performance/2026-10-03-replica-accounting-generation/README.md).

## Remaining delivery boundaries

These interfaces do not authorize role changes or publish a Runtime generation.
The default Replica still builds historical State and uses the legacy adapter;
Primary/Standalone default startup also remains unmigrated. Live structural-only
roll application is locally implemented for both selected disk and legacy State
owners. Startup normalization and coherent role generation publication still need
the planned selected-owner handoff and maintenance protocol.

Fresh build and continuation have history-dependent IO. Existing native
`Log.segments`, Usage history, each-record projection transactions, shared
RAM/disk fees, whole migration, and D-03 cross-restart content/key-set trust still
need their planned implementations and acceptance. There is no production
snapshot seal, incremental-startup guarantee or Linux capacity claim here.

Local verification: [permitted-prefix and disk Replica evidence](../verification/performance/2026-10-03-replica-projection-prefix/README.md).

Additional local verification: [confirmed physical roll and retry barriers](../verification/performance/2026-10-03-replica-roll-projection/README.md).

Additional local verification: [role-bound fresh owner construction](../verification/performance/2026-10-03-replica-owner-construction/README.md).

The composite disk Usage follow-up is verified separately in
[dual-DB and Usage projection evidence](../verification/performance/2026-10-03-usage-disk-projection/README.md).
Older construction evidence refers to its saved compiled baseline, which still
used the resident Usage bridge at that stage.
