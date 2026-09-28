# HA durable transition evidence contract (proposed)

Status: version-3 state codec, member-local journal, recovery, low-level
idempotent offline migration helper, runtime version dispatch and an
authenticated bounded member paging endpoint and independent paged collector
are implemented under package tests. An explicit offline operator migration
command and append-only member and collector segment rotation are available.
Verified archival deletion and an external immutable sink are **not
implemented**. Newly
established and currently unmigrated members still write version-2 state; no
automatic migration occurs. This contract covers authenticated
`role`/`term`/`promised_term` transitions of one member in one incarnation.
It does not turn coordinator availability, Replica apply stage, an operator's
fencing assertion, or the final client response into durable facts.

## Why the current sources are incomplete

`StatePublisher.publishTransition` fsyncs `cluster/state.json`, then appends
an event to a process-local ring. A crash in between leaves the new durable
role or term without the event. Offline `cluster promote` subsequently appends
`cluster.promote` Audit; a crash between promotion and that append leaves the
same gap. The independent health service persists only events it has polled.
It therefore cannot repair a transition that happened while it was offline.

The authenticated member state remains the safety authority. A new event
source must never substitute an event for the state, alter election rules, or
allow a stale event to authorize promotion.

## Required durable protocol

1. Upgrade the authenticated member state to a new version with a journal
   identity and committed `(sequence, record_digest)` cursor covered by the
   state MAC. Progress publications preserve this cursor. This is how startup
   distinguishes a missing required journal from a legacy member that has
   never had one; a companion file alone cannot prove its own absence.
2. Under the existing member data lock and the `StatePublisher` mutex, append
   one bounded, authenticated write-ahead transition record to a member-local
   journal **before** publishing a changed `state.json`. The record names the
   cluster, incarnation, member, monotonically increasing sequence, fixed
   transition kind, prior and next `(role, term, promised_term)` tuple, UTC
   occurrence time, and previous record digest. Domain-separate the MAC from
   the state MAC with the derived cluster key. Do not record keys, tokens,
   addresses, free-form error text, or operator credentials.
3. The journal record and its directory entry must be durable before the
   existing state file fsync/rename/directory-fsync barrier begins. The new
   state cursor then commits precisely that record in the same authenticated
   state publication as the role/term/promise change. A failed
   journal write leaves state unchanged. A state write failure after the
   journal barrier is **ambiguous** because rename may have succeeded before
   directory fsync failed: the current publisher must reject further
   transition/progress calls until it has authenticated and reconciled the
   on-disk state and journal. It must not acknowledge a promise or promotion
   from memory alone.
4. At startup, authenticate the state and every journal header/record in
   order. The state cursor identifies the exact committed prefix. One final
   record beyond that cursor whose `prior` tuple equals current state is
   uncommitted and may be retried or explicitly discarded only by recovery
   code. Any other mismatch, gap, duplicate sequence, broken digest/MAC,
   identity change, extra pending records, or missing required journal is an
   error requiring operator recovery; do not silently report a complete
   timeline. Progress-only state publications preserve the transition tuple
   and do not create transition events.
5. Recovery must reconstruct the committed transition history and next
   sequence before opening a role-specific runtime or serving `/ha/status`.
   A process restart is not a sequence reset. Only committed records are
   exported. A crash after the state barrier but before the method returns
   must reappear as one committed transition after restart; a crash before
   state publication must not appear as a committed transition.
6. Do not truncate the only full history to the current 128-event process ring
   or the monitoring service's 1000-record cache. If retention is bounded,
   use authenticated closed segments plus a durable manifest/anchor and a
   verified archival handoff before deleting old segments. Report the first
   retained sequence and any declared prehistory or loss; never label an
   incomplete range "complete". The `/ha/status` response may remain bounded
   and paginated, while an authenticated archival reader owns older segments.

The member now creates a new private, fsynced segment before publishing the
first transition that would exceed 64 MiB in the current file. Segment names
encode their first sequence. Startup authenticates the baseline and every
segment in order; a missing, reordered, malformed or tampered segment refuses
startup. A crash that leaves the first record of a new segment uncommitted is
reconciled against the authenticated state cursor, and the empty pending
segment is removed durably. The member never deletes a closed committed
segment. There is therefore no archival-deletion manifest or handoff yet;
disk use and recovery I/O grow with the full history, while the sparse seek
index grows with the number of checkpoints and segments. A future pruning
protocol must add authenticated anchors and independent archival proof before
removing any closed segment.
All members expose their authenticated state format version; version-3
members additionally expose the retained segment count and known journal
bytes as unlabelled Metrics gauges. Capacity accounting reads the current
active file size against the publisher's segment ledger; an inconsistency
removes the size samples and sets the capacity-readable gauge to zero.
These are local growth signals, not free-volume or inode measurements and not
an independently authenticated archive. Version-2 members omit them.

## Reader and collector contract

Keep today's `live_transitions` explicitly process-local. A separate,
versioned durable source must expose a chain identity
`(cluster_id, incarnation, node_id, journal_version, baseline_digest)`, the
baseline type, first available and last committed sequence/digest, and its
completeness status. A bounded event page must accept an authenticated cursor
and report the next cursor; the reader must reject an inconsistent cursor or
request for a sequence no longer retained. Do not expose the raw journal,
cluster key, or record MAC to the browser. The independent collector must
verify every page's identity, sequence, prior/next tuple and digest continuity
before persisting it. It must catch up all pages after a monitoring outage,
instead of treating one 128-event status response as a full history. A
`legacy_baseline`, missing segment, verification error, or unfinished catch-up
must remain visible as an incomplete interval in the page and evidence export.

The member now serves `GET /ha/transitions` on its Metrics listener under the
same mTLS certificate, separate HA bearer, admission limit and access audit
as `/ha/status`. The fixed page size is at most 64 committed events. The first
request has no cursor; continuation sends the returned `after` sequence,
`digest` and `journal_id`. A changed or forged cursor is rejected. The
response includes chain identity, legacy/initial baseline kind, committed
head, page events and `has_more`. Continuity digests are SHA-256 hashes of
private record MACs, not the MACs or cluster key. Version-2 members return an
unavailable response; no caller may interpret that as an empty complete chain.
The independent health service can now request, validate, and persist these
pages in a separate private file. It resumes from a stored member cursor after
restart, checks every page's identity, sequence, digest predecessor and
role/term tuple, and marks startup, failures, stale reads and unfinished
catch-up as incomplete. `GET /api/durable-transitions` and the evidence export
show per-member coverage through the last **observed** committed head. A
legacy baseline leaves all earlier transitions unknown. The collector does
not independently authenticate the member's private MAC; it relies on the
member's verified journal plus its mTLS/bearer channel and checks continuity
of the published MAC digests. The collector atomically maintains a private
cursor manifest and consecutively numbered closed segments. It rotates before
the next nonempty page would push the manifest past a 3 MiB target; each
document has a 4 MiB hard limit. Startup verifies every segment and its
continuity with the manifest, refuses missing or inconsistent segments, and
completes a matching segment-written/manifest-not-yet-updated rollover. The
earlier unsegmented collector file is upgraded in place after validation.
Replay reads one bounded segment at a time and keeps only chain cursors and
the latest 20 UI events per chain; it does not load all closed event payloads
at once. The number of segments still determines recovery I/O, and retained
incarnation metadata still grows with the chain inventory.
All closed segments are retained; `journal_full` still stops cursor advancement
if one page or the manifest metadata exceeds the hard limit. The collector's
local documents have no independent cryptographic authentication or external
immutable sink. Capacity, archival handoff and production-grade complete
retention remain pending.

The health service exposes `closed_segments`, `manifest_bytes`, and
`storage_bytes` in the operator-only durable-transition response and evidence
export. These measure its current local retained files, not remaining free
space, a storage quota, or an independently verified archive. The page shows
the manifest's share of the 4 MiB per-document bound. A deployment must
separately monitor persistent-volume capacity and inodes and measure replay
time at its expected retention before enabling production retention claims.
An offline `-verify-durable-snapshot` command can check a frozen collector
copy without repair, returning per-file SHA-256 and a deterministic ordered
inventory hash. It rejects an active collector lock and unfinished rollover.
The hash becomes durable archive evidence only when an independent immutable
sink stores both the exact files and that root and later re-verification
matches them. This check has no member MAC key, cannot prove live catch-up,
and does not authorize deleting old member or collector segments.

The collector's frozen-snapshot report also carries each chain's last
observed committed head digest and observation time. With
`-compare-member-snapshot-reports`, the offline verifier compares private
member verifier reports against the frozen collector's latest chain for every
expected member. It requires identity, baseline, committed head, stored
cursor and the ordered committed event projection hash to match exactly,
and returns `snapshot_heads_match` with both file
inventory roots. This is a report-consistency result, not re-authentication
of member bytes: member reports must first be regenerated from the exact
archived member files with the Master Keys. It does not prove an atomic
cross-host freeze, historical incarnations, external retention or safe
deletion.

With `-verify-member-snapshot-manifest`, the offline health tool instead
unlocks each member's Master Key on a hardened process and re-verifies the
frozen member state and full MAC journal itself. It compares both the head
and the ordered public projection of every committed event in that current
incarnation against the collector's full retained segments. It returns
`member_authenticated_current_chain_match` and the member file inventories.
This removes report-file substitution and detects modified collector event
fields even if the opaque head digest is unchanged. It cannot authenticate
collector records for earlier incarnations without their corresponding
frozen member copies. Exact archived bytes, independent receipts and
target-environment replay remain separate gates.

A version-2 direct member manifest lists every `(node_id, incarnation)` in
the verified collector inventory, each with its own frozen directory and
matching config. The offline command then returns
`member_authenticated_all_collected_chains_match` only when all expected
members have a chain and every collected incarnation matches an authenticated
member head and ordered event projection. Omitted historical copies,
duplicate paths, an incomplete collector cursor and wrong identities fail
closed. Its scope is all **collected** incarnations from each authenticated
baseline, not history predating a legacy baseline or proof of an external
immutable archive.

After a same-incarnation reseed, the direct member manifest must be version 3
and identify each frozen generation by `(node_id, incarnation, journal_id)`.
The all-chain comparison also checks the handoff's old/new member inventory
hashes, old event root and new baseline digest. This still depends on retaining
the exact frozen member copies and separate immutable receipts.

`events_sha256` is SHA-256 over the current incarnation's committed public
events, in sequence order from the baseline. Each event is encoded as one
UTF-8 JSON line using the `DurableTransitionEvidence` field order
(`sequence`, `at`, `kind`, `from_role`, `to_role`, `from_term`, `to_term`,
`from_promised_term`, `to_promised_term`, `previous_digest`, `digest`) and a
single trailing LF. An empty committed sequence has the SHA-256 of empty
bytes. This binds the collector's retained event fields to the authenticated
member projection; the member's private record MACs are not exported.

The separate `halro cluster verify-transition-snapshot` command checks a
frozen copy of one member's version-3 `state.json` and every transition
journal segment against the matching member configuration and Master Key. It
uses read-only file handles, authenticates the state and full record MAC chain,
hashes the ordered public projection of all committed events, requires the
state cursor to match the committed journal head, and refuses an
uncommitted intent rather than repairing it. Its ordered file inventory
includes per-file SHA-256 and `inventory_sha256`; the output exposes public
digests of the baseline and committed record MACs, never the MAC or key itself.
The operator must stop the member or take an atomic snapshot before the copy:
this command cannot establish that a copy was frozen, nor prove quorum,
fencing, ordering journal contents, application state, collector catch-up or
external retention. A `legacy_baseline` still leaves earlier transitions
unknown. An independent immutable receipt and matching verification of the
archive copy are required before calling the member handoff complete. Neither
local verifier authorizes evidence deletion.

## Upgrade and identity boundaries

An unmigrated `state.json` is version 2, authenticated, and has no transition
cursor. Existing members use an offline, data-lock-protected migration that
authenticates version 2 state, durably writes an explicit `legacy_baseline`
anchor, then publishes version 3 state with that anchor identity/cursor. A
crash before the version 3 publication leaves version 2 authoritative and
retries migration against the same verified baseline; a crash afterward must
find the journal or refuse startup. Earlier transitions remain unknown.
Freshly seeded members can start with an initial-state anchor. A reseed or
new incarnation starts a new chain and must never append to the old
incarnation's identity. Mixed binaries must refuse journal formats they
cannot verify; rollout sequencing and rollback behavior require their own
acceptance test before changing the state format or enabling the journal by
default.

### Same-incarnation member reseed is a separate evidence handoff

Reseeding one member does **not** change the cluster incarnation. The old
member's version-3 journal may contain committed transitions after the
collector's last poll, including a demotion before `reseed-required` stops its
runtime. `seed-install` intentionally publishes a version-2 Replica state;
the explicit offline migration then creates a new version-3 journal ID and a
new baseline. The old and new journals have the same `(cluster, incarnation,
node_id)` but are distinct evidence chains. A matching ordering index or a
valid new seed manifest cannot silently splice their transition digests.

Until a verified handoff exists, the collector must retain its old cursor,
mark that member unavailable, and diagnose an authenticated changed journal
as `journal_changed_requires_reconciliation`. It must not reset the cursor,
erase the old chain, accept the new baseline as a continuation, or call the
member's durable history caught up. The diagnosis does not establish that the
change was an authorized reseed; a tampered or replaced member needs the same
quarantine.

`halro cluster verify-transition-snapshot` can read a bounded committed tail
from a **frozen** retired member copy when supplied `--after-journal-id`,
`--after-sequence` and `--after-digest` from the collector's exact old cursor.
It verifies the member state and full MAC journal, rejects a wrong cursor, and
returns the verified file inventory with up to 64 events and the next cursor.
An operator must retain every page and check that successive inventories are
identical. The health binary's offline
`-reconcile-retired-member-snapshot` mode can then import this authenticated
old tail into a **stopped** collector using its exclusive journal lock. It
rechecks the frozen member copy per page and will only append events continuous
with the collector's existing old cursor. It returns the exact member inventory
hash and the status `retired_member_tail_imported_new_generation_unapproved`.
Importing an old tail does **not** approve the replacement journal, fill a
missing source interval outside that frozen copy, or complete the handoff.

The offline preflight now authenticates the frozen retired member against the
collector's complete old chain, the stopped source Primary against the signed
seed approval and exact authoritative files, and the replacement's new journal
baseline against the precise version-2 Replica state installed by that seed.
It also verifies the seed ordering prefix and replacement ordering head. Its
`authenticated_inputs_match_handoff_uncommitted` result only checks those local
inputs. Freeze times and the fencing evidence digest are operator-supplied
references; the preflight does not independently certify fencing or change the
collector.

The stopped-collector `-commit-reseed-handoff` mode now requires that preflight
again, compares the live collector's exact files with the frozen preflight copy,
and atomically writes a version-3 manifest containing the intact old chain, a
handoff record, and the new baseline. Replay rejects duplicate generations
without a matching record, a changed old cursor, or a changed old event-sequence
hash. The offline snapshot verifier enumerates both generations and a version-3
member manifest can authenticate each `(node_id, incarnation, journal_id)`
directly. Local file replay and copy-based handoff tests establish this repository
mechanism. The dedicated local kind collector was also stopped, backed up,
reconciled against a newly frozen copy, switched to format 3, and restarted;
its API resumed the replacement journal after a second service restart.
All four collected member generations were directly reauthenticated from
frozen local copies and matched to that collector snapshot. Independent
immutable archival receipts and readback from that separate store remain
acceptance steps. An irrecoverable old-journal interval
still blocks a complete handoff: this command requires the collector to match
the retired member's final authenticated head.

The handoff record binds the old and new journal IDs, authenticated old head
and event root, target identity, source term/index, seed manifest digest, and
operator-recorded freeze/fencing references. Collector replay and offline
snapshot verification index generations by `(node_id, incarnation, journal_id)`
and reject a duplicate without a matching handoff. The current commit mode
requires **no old-journal gap**; an irrecoverable interval cannot be represented
as a complete chain. Exact bytes and the handoff record still need independent
immutable receipts and readback before production retention acceptance.
Rotating the collector file to hide a conflict is not an evidence handoff and
does not authorize deletion of either member journal.

The repository can encode, authenticate and read a version-3 state cursor
without altering the version-2 golden encoding. `NewDurableStatePublisher`
opens an authenticated journal and rejects missing, corrupt or inconsistent
records; it recovers a final intent that was not committed by state.
`MigrateStateTransitionJournal` can resume from an authenticated legacy
baseline after a crash. That baseline binds the MAC of the **entire** version-2
state, including progress and membership, so an intervening old-binary write
cannot be mistaken for the same migration point. The `halro cluster
migrate-transitions` command holds the member data lock, checks the inspected
incarnation/role/term/promise and all three indexes, reauthenticates an administrator,
invokes that low-level API, then reopens the authenticated state and full
journal before reporting success. It is idempotent for an already-migrated
member at the same inspected tuple. The normal runtime dispatches by
authenticated state version and never upgrades a member on startup. The
ordinary version-2 publisher refuses a partial migration journal, including
orphaned segments. Individual member segments are limited to 64 MiB and
rotate without deleting old evidence. Production activation still requires
bounded capacity, archival handoff, latency and recovery gates.

The journal's `promote` record proves a durable member role/term publication.
It does **not** prove quorum promises, external fencing, `cluster.promote`
Audit completion, or that the new Primary started serving. Those have distinct
evidence sources and must be correlated by cluster/incarnation/member/term.

## Failure-injection gate before implementation is considered delivered

| Injection point | Required recovered result |
|---|---|
| Journal create/write/fsync/directory fsync fails | Previous state remains authoritative; no promise or promotion ACK; no committed event. |
| Journal durable, state temp write/fsync fails | State stays old; final journal record is pending, not exported as committed. |
| State rename succeeds, directory fsync or process fails | Startup authenticates disk state and journal, then either recovers the one committed event or refuses inconsistent storage. |
| State durable, process dies before in-memory ring or Audit append | Restart exports the durable transition exactly once; Audit may separately be missing. |
| Repeated promise/retry and progress writes | Sequence remains contiguous; no duplicate transition and no event for pure progress. |
| Torn/truncated/reordered/tampered journal or wrong cluster/incarnation | Startup refuses a complete-chain claim and does not serve a false healthy source. |
| Old version 2 member, migration, rollback, reseed | Prehistory remains explicitly unknown; old and new incarnation chains cannot merge. |
| Segment rotation, archive lag or loss | Retention boundary and gaps remain visible; deletion requires verified handoff. |

Performance and availability are part of this gate: measure the extra durable
write latency on the prepare and promotion paths, the behavior under disk-full
conditions, and recovery time with the configured retention. No production
claim follows from package tests alone; target-environment fault injection,
restart, mTLS readout, and independent archival verification remain required.

The member now authenticates each record in a bounded streaming startup scan
and retains the baseline, authenticated committed head, at most one pending
intent and one seek checkpoint per 64 records. `Page` reopens the requested
segments on a separate read descriptor and authenticates a bounded range; a
changed requested record returns an unavailable error instead of a false
empty page. Pending-intent recovery copies only the active segment prefix
before atomic replacement, without reconstructing all historical records.
The optional bulk `CommittedEvents` helper now returns read errors; the
machine endpoint continues to use bounded pages.

The local Apple M4 benchmark using 8 KiB test segments measured cumulative
allocation per open at 1.06 MB before and 0.81 MB after for 256 transitions
across 16 segments, and 9.22 MB before and 6.43 MB after for 2,048 transitions
across 125 segments. After-refactor recovery times were 1.52 ms and 23.88 ms
in one `-benchtime=2x` run. Full MAC verification still scans every record.
These are local cumulative allocations, not peak retained heap, production
latency or an acceptance pass. Measure startup retained heap, replay time and
cross-segment page latency at the expected retention size before deployment.
