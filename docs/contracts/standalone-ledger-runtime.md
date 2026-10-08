# Standalone default Ledger owner

The default Standalone Runtime selects a fresh disk Ledger projection for Budget
admission, recovery, price-pin repair and accounting readers. Primary now selects
its own fresh business disk owner with confirmed asynchronous startup recovery
([handoff](primary-ledger-runtime-handoff.md)); Replica selects an applied-cut
disk Ledger/Usage generation ([handoff](replica-disk-runtime-handoff.md)).
The WAL remains the accounting authority.

## Startup and recovery

`dataDir/ledger-projection.db` is a reserved node-derived file. Open holds the
exclusive data-directory lock, discards a previous derivative, freezes native
writes with a maintenance lease, and rebuilds from the complete authenticated
native history into an exclusive fresh DB. The selected head and hash must equal
the frozen native head. This is full-history transitional startup, not trusted
snapshot or incremental startup.

Before startup changes files, and again under the directory lock before rebuild,
Open rejects configured Metadata, master/KMS/TLS/credential and other File/Path
collisions with the reserved path, including existing hardlink/symlink aliases.
The reserved name must not hold operator-managed data. This check does not
authorize deleting arbitrary paths or protect against an external process that
ignores the directory lock and races a file replacement.

The maintenance lease spans construction, price-pin repair and two-level
recovery. Recovery separately pages all pending attempts and unfinalized
requests, with 64 rows/8MiB limits. Unstarted attempts release their lease;
started attempts settle conservatively from frozen evidence. A request without
pending attempts still receives its deterministic terminal fact. Existing
terminal facts are preserved. Usage catches up synchronously after recovery and
before business/background publication.

Old WAL without RequestAccepted retains the actual first unattributed lease as
its recovery origin; no accepted event is fabricated. That origin persists after
settlement, so interruption between settlement and request finalization can be
retried. Original terminal facts can precede a legacy lease or a later actual
Accepted. Each legacy lease retains its own project/period accounting boundary;
modern accepted requests keep strict attribution, including WorkUnit/Run.

## Usage first-fact semantics

Default Usage now borrows the selected projection through its disk derivative.
Earlier reader-bound Aggregate/checkpoint support retains the same first-fact
semantics, but does not authorize default saved-checkpoint startup. Request
events consult the first actual terminal's sequence;
a projection ahead of Usage does not make an earlier event late. Events after a
processed terminal can update attempt accounting without reopening an active
request or rewriting its original summary. A distinct repeated terminal does
not add a second request summary. Explicit legacy origin evidence comes from
that same Ledger owner, including through LedgerDerivative and disk Usage
prefetch; lookup failures remain errors.

Reader-bound checkpoints carry `reducer_semantics=ledger-first-facts-v1`.
An unmarked checkpoint can contain duplicate terminal totals even with no active
requests. Runtime rejects it, clears checkpoint and daily rollup together, and
rebuilds both from Genesis with the selected reader. Reader-mode checkpoints
also cannot be restored under plain reducer semantics. This does not authenticate
checkpoint content or grant cross-restart trust to the Ledger derivative.

## Lifetime and failure

Runtime retains the physical DB independently of its diagnostic reader alias.
Shutdown withdraws and joins real Manager/Collector/transport operations before
closing the projection. The data-directory lock stays held through failure
cleanup and actual DB reader exit. Close removes the derived path only when its
current file identity matches the owned file; replacement files survive with an
explicit error. A failed authenticated build is not published as empty state.

## Remaining delivery conditions

Standalone default Usage and Governance now use fresh disk owners. Primary
Governance State, remaining HA/maintenance generation publication,
shared RAM/disk/unconfirmed owner fees, startup space
and maintenance-window gates, D-03 authenticated incremental recovery, migration,
backup/upgrade fault matrices and target Linux capacity acceptance remain open.
Local tests of this default path do not approve the final storage layout or
establish production acceptance.
