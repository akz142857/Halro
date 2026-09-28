# HA member operations

This runbook covers the repository-supported manual HA lifecycle. It does not
authorize a production rollout: the admission evidence in
`docs/todo/halro-ha-architecture.zh-CN.md` §1.3 remains a separate gate.

## Invariants before any operation

- Stop the member named by `--config` before running an offline command. A
  failure to acquire the data lock is a refusal, not a prompt to delete it.
- Read `halro cluster status` and record cluster ID, incarnation, role, term,
  promised term and all three indexes. Promotion and planned handoff require
  `durable_index == confirmed_index == applied_index` on the candidate.
- Never edit or copy `cluster/state.json` or `cluster/ordering.journal`.
- Keep the same Master Key on every member, but use a distinct node
  certificate and the configured SPKI pin for every peer.

## Inspect member status

The Primary Admin Console has a read-only **Cluster status** page. It reports
the local authenticated role, term and replication indexes, plus whether this
member has a data session with each configured peer. A connected peer is not
proof that its projection has caught up. For a promotion decision, run
`halro cluster status --config /etc/halro/config.yaml` on **each** member and
compare their authenticated indexes. The Replica Admin API also serves its
own authenticated `GET /admin/api/v1/cluster/status`; the normal Console is
served from the Primary. An unreadable status is unknown, never a zero index
or a healthy cluster.

For a repeatable validation sequence and observable pass criteria, see
[`ha-test-guide.zh-CN.md`](../verification/ha-test-guide.zh-CN.md).

## Reconcile an uncertain Admin write

An Admin mutation returning `503` with `code: replication_unavailable` has not
passed its required confirmation barrier. It may already have changed the
Primary's local durable state and can become confirmed when a Replica returns.
The same uncertainty applies to a request timeout or a broken connection after
the mutation was admitted. Record the original response, request identity and
precondition; do not treat the error as proof that no mutation occurred.

After peer recovery, check the Primary's confirmed watermark and read the
resource and revision through authenticated Admin API before deciding whether
to retry. Compare the actual fields with the intended update and inspect the
Admin audit record. Preserve the original `If-Match` revision for an update or
the original idempotency key for a create; do not create a second operation
while the first outcome is unresolved. Escalate a conflict or a missing audit
record instead of inferring the result from HTTP status alone.

## Offline transition-journal migration (gated trial)

The new binary can read version-2 and version-3 member state. This command
changes a stopped member's authenticated state to version 3 and creates a
`legacy_baseline`: migrations before that instant remain unknown. The member
journal rotates private 64 MiB segments and retains every closed segment; it
has no verified archival deletion or disk-capacity policy. The independent
collector rotates private segments at a 3 MiB target and retains every closed
segment, with a 4 MiB per-document hard limit. **Do not use this command
as production HA evidence retention** until archival handoff, capacity,
recovery and target-environment
acceptance gates pass.

Record `cluster status` for every member, stop all members and fence client
traffic, then capture and verify a full stopped backup. Confirm that every member
will restart on a binary that understands version 3. Then run this command on
each stopped member using its own config and its exact inspected values:

```sh
halro cluster migrate-transitions --config /etc/halro/config.yaml \
  --expect-incarnation inc_... --expect-role replica \
  --expect-term 7 --expect-promised-term 7 \
  --expect-durable-index 42 --expect-confirmed-index 42 \
  --expect-applied-index 42 \
  --username admin --password-file /secure/admin-password
```

Supply `--totp-file` when MFA is active. The command refuses a held data
lock, changed state or failed administrator authentication before writing. A
failure after journal creation or state publication may be ambiguous; leave
the member stopped and rerun the same command to authenticate and resume the
same baseline. Do not delete the journal or hand-edit state. After success,
`cluster status` must report `state_version: 3` and a nonempty
`transition_journal_id` for each migrated member; verify the machine-only
`/ha/transitions` baseline and independent collector before admitting client
traffic. `cluster status` authenticates state but does not by itself replay
the journal. An old binary cannot safely open version-3 state. After members have
resumed and emitted transitions, rolling back requires a separately verified
whole-cluster recovery plan, not copying back version-2 `state.json`.

## Frozen independent collector evidence check

Stop `halro-ha-health` or take an atomic filesystem snapshot of its private
persistent directory. Preserve `durable-transitions.json` and **every** file
with the `.segment.00000000000000000000` style suffix in one snapshot; do not
make a live file-by-file copy. Verify the frozen copy with the same environment,
cluster and complete member inventory used by the collector:

```sh
halro-ha-health \
  -verify-durable-snapshot /secure/ha-snapshot/durable-transitions.json \
  -environment production -cluster cluster-a \
  -members halro-0,halro-1,halro-2 \
  > /secure/ha-snapshot-inventory.json
```

The command is read-only. It refuses an active collector lock, old file
version, unfinished rollover, missing segment, bad permissions, changed
identity or broken stored chain. Its JSON lists each file's size and SHA-256,
an ordered `inventory_sha256`, every stored chain's last observed cursor and
members with no captured baseline. Store the snapshot and inventory root in
an independently controlled immutable archive, then verify the archive copy
and compare the root and chain cursors before calling the handoff complete.
`local_files_verified` proves only that the frozen local files agree with each
other; it does not authenticate the member's private MAC, prove the collector
was caught up to the live member, establish external retention or permit
deleting any member or collector segment. Keep original evidence on failure.

## Frozen member transition evidence check

For each member, stop Halro or take an atomic filesystem snapshot, then copy
its complete `cluster/state.json`, `cluster/transitions.journal`, and every
`cluster/transitions.journal.segment.*` into a private snapshot data directory.
Keep the member's matching configuration and Master Key available to the
verifier; Key Slots mode also needs the snapshot's metadata store. Do not use a
live file-by-file copy. With the member stopped or the copy frozen, run:

```sh
halro cluster verify-transition-snapshot \
  --config /etc/halro/config.yaml \
  --snapshot-dir /secure/halro-0-snapshot \
  > /secure/halro-0-transition-inventory.json
```

The snapshot directory must be a clean absolute private path separate from
the configured live data directory. The command reads and authenticates the
version-3 state and full member transition MAC chain without repairing or
writing files; it refuses an uncommitted intent, missing or altered segment,
bad permissions, or identity mismatch. Save its exact files and
`inventory_sha256` with an independently controlled immutable receipt, then
verify the archive copy and compare the root and committed cursor. Record the
freeze time and the corresponding collector cursor to distinguish a complete
member snapshot from a collector that has not caught up. A
`member_mac_verified` report only covers member state and transition journal
files. It does not prove the member's ordering or application state, quorum,
fencing, client results or external retention; a legacy baseline does not
reconstruct earlier events. Keep old segments until a separately accepted
retention and recovery procedure authorizes deletion.

After separately re-verifying the archived collector and **every current
member** snapshot, place the member verifier JSON outputs in private files
and create a private manifest such as:

```json
{"version":1,"reports":["/secure/halro-0-transition-inventory.json","/secure/halro-1-transition-inventory.json"]}
```

Compare those reports against the archived collector files with the same
environment, cluster and complete member list:

```sh
halro-ha-health \
  -verify-durable-snapshot /secure/ha-snapshot/durable-transitions.json \
  -environment production -cluster cluster-a \
  -members halro-0,halro-1 \
  -compare-member-snapshot-reports /secure/member-reports.json \
  > /secure/transition-head-comparison.json
```

`snapshot_heads_match` requires the collector's latest chain for each member
to have the same incarnation, journal, baseline, exact committed sequence,
digest and ordered event-sequence hash as that member's frozen state; the
stored cursor must have reached that head. The comparison includes both
inventory roots and hashes of the member report files. It compares reports,
so it cannot authenticate a report
that was forged or disconnected from its archived member bytes. Re-run the
member verifier on the exact archived bytes, keep both reports and roots with
independent immutable receipts, and compare again after readback. The match
only concerns the current member incarnation at the snapshot point; it does
not prove older incarnations, simultaneous freeze, live catch-up after the
snapshot, retention, or permission to delete any segment.

When the secure verification host can access the matching member configs and
Master Keys, prefer direct verification of the archived bytes. Supply a
private `0600` manifest with one entry per expected member:

```json
{"version":1,"members":[{"node_id":"halro-0","config":"/secure/halro-0.yaml","snapshot_dir":"/secure/halro-0-snapshot"},{"node_id":"halro-1","config":"/secure/halro-1.yaml","snapshot_dir":"/secure/halro-1-snapshot"}]}
```

Run the same collector snapshot command with
`-verify-member-snapshot-manifest /secure/member-snapshots.json` in place of
`-compare-member-snapshot-reports`. This mode hardens the process before
unlocking Master Keys, re-authenticates every member's state and full journal
MAC chain, and includes the member file inventories in its output. The
archived config may name an original data directory that is no longer
mounted, but its member identity and Master Key source must still match the
frozen copy; Key Slots mode also needs the copied metadata store. The result
`member_authenticated_current_chain_match` proves the current incarnation's
head and every retained collector event tuple match the MAC-authenticated
member journal projection. It does **not** authenticate earlier incarnations
without their archived member copies, prove an atomic cross-host freeze or
certify external retention. Preserve both sets of exact files and the output
under independent immutable receipts; do not delete evidence on this result
alone.

To check every incarnation retained by the collector, use manifest version 2
with one entry for **each** collected `(node_id, incarnation)` pair, including
the current ones. Each entry names that incarnation's frozen member directory
and matching config and Master Key source:

```json
{"version":2,"members":[{"node_id":"halro-0","incarnation":"inc-old","config":"/secure/halro-0-old.yaml","snapshot_dir":"/secure/halro-0-old"},{"node_id":"halro-0","incarnation":"inc-current","config":"/secure/halro-0.yaml","snapshot_dir":"/secure/halro-0-current"},{"node_id":"halro-1","incarnation":"inc-current","config":"/secure/halro-1.yaml","snapshot_dir":"/secure/halro-1-current"}]}
```

Pass this file to the same `-verify-member-snapshot-manifest` flag. The result
`member_authenticated_all_collected_chains_match` requires every expected
member to have a collected chain, every collector chain to reach its matching
frozen member head, and every committed event projection to match. A missing
historical snapshot, repeated directory, wrong incarnation or incomplete
collector chain rejects the whole result. This covers the collector's
retained incarnations from their own baselines; it cannot recover history
before a legacy baseline or certify external retention and safe deletion.

For migrated v3 members, query
`halro_replication_transition_journal_segments` and
`halro_replication_transition_journal_bytes` per expected member. A zero
`halro_replication_transition_journal_capacity_readable` means local capacity
accounting failed; preserve the data directory and investigate the active
file before relying on its size. Require exactly one current readable,
`segments` and `bytes` sample aligned with that member's successful
`up` scrape; a missing or duplicated capacity gauge triggers the same
capacity alert after one minute. Check raw `/metrics` and metric relabeling
before treating this case as disk corruption. Missing series on a v2 member
mean the durable transition journal is not enabled. Monitor persistent-volume
free bytes and inodes separately, estimate growth against the site's retention
budget, and rehearse full-chain startup and archived-copy readback at the
expected largest inventory. These gauges do not justify deleting old files.

## Initial Primary

Create and verify the ordinary data directory, add the `replication` block,
stop Halro, then establish the one initial Primary:

```sh
halro cluster establish --config /etc/halro/config.yaml \
  --role primary --incarnation inc_... --term 1
```

`cluster establish --role replica` is deliberately refused. Every Replica
must enter through the approved seed path below.

## Seed a Replica

1. Stop the fully caught-up Primary. Generate an approval manifest while
   reauthenticating an administrator (and supplying `--totp-file` when MFA is
   active):

   ```sh
   halro cluster seed-approve --config /etc/halro/config.yaml \
     --target halro-1 --output /secure/halro-1.seed.json \
     --username admin --password-file /secure/admin-password
   ```

2. Copy the stopped Primary's authoritative snapshot to a private sibling of
   the target `storage.data_dir`. Include `cluster/ordering.journal` and
   `cluster/provider-object-sources/`, but exclude
   `cluster/state.json`, `cluster/maintenance`, `cluster/totp-watermark`, and
   `.halro.lock`. Use an authenticated encrypted transport; the manifest is
   integrity and approval evidence, not confidentiality.
3. Preinstall the same Master Key and the target node's own TLS material. Run:

   ```sh
   halro cluster seed-install --config /etc/halro/config.yaml \
     --staging /var/lib/halro/.seed-staging \
     --manifest /secure/halro-1.seed.json
   ```

The installer requires staging to be a sibling of `storage.data_dir`, verifies
the manifest MAC, the complete ordering chain at the exact approved index,
and every authoritative file, rejects source-local member state, writes the
target Replica state inside staging, then publishes the directory with one
rename. Failure before publication leaves the target absent.

Index `0` is valid only for the initial seed before the Primary has committed
its first globally ordered frame. It authenticates the empty ordering prefix;
after ordered work exists, the manifest preserves that non-zero index exactly.

## Planned handoff

Stop the caught-up target Replica and run the handoff from that target. The
old Primary must still be reachable on the replication channel:

```sh
halro cluster stepdown --config /etc/halro/target.yaml \
  --from halro-0 --to halro-1 --expect-term 7 --expect-index 10241 \
  --username admin --password-file /secure/admin-password
```

The old Primary withdraws readiness and drains every admitted HTTP handler
before it freezes the ordering prefix or signs the higher-term promise. If a
handler completes an ordered write during that drain, the checked index has
advanced: the command fails before the promise. Let the target catch up, read
the new index, and retry with that index; do not treat the stale-index refusal
as a completed handoff.

The old Primary durably promises the higher term and exits. The target writes
`leadership_established`, `cluster.stepdown.requested`, and
`cluster.stepdown.completed` into its authenticated suffix and does not become
ready until that term is replicated and confirmed.

Start the promoted target and wait for its `/health/ready` to return 200.
Then start the former Primary as a Replica and check all members separately:
one Primary in the new term, authenticated `durable >= confirmed >= applied`
on each member, and eventual equality of the applied indexes. A former
Primary that refuses its metadata projection check needs investigation or a
fresh approved seed; never edit `state.json` to force it online.

## Unplanned manual promotion

Fence the old Primary outside Halro first. Then stop the fully caught-up
candidate and run `cluster promote` with the observed term/index and the exact
fencing assertion. `--no-peer-promise` is accepted only for a two-member
cluster and leaves the promoted member unable to confirm new writes until its
peer returns. `--self` is only for a stopped Primary explicitly
re-establishing itself in a newer term.

Keep the fenced old Primary offline until the new Primary has established and
confirmed its term. If the old PVC is started, its startup adjudication must
withdraw readiness and refuse old-term authority. A `reseed-required` marker
means the old directory has an incompatible suffix; preserve its complete
bytes and the refusal log, then stop the Pod again. Do not remove the marker,
edit `state.json`, or change the role by hand. To rebuild that member, stop the
new fully confirmed Primary and follow **Seed a Replica** above with a fresh
approval at its exact term/index. Retire the old target data directory without
overwriting it, install only the approved source files into a sibling staging
directory, and migrate the newly installed version-2 Replica to version 3
offline before restarting it. Compare all members' authenticated state and
applied prefixes after it rejoins.

A reseed within the same cluster incarnation creates a **new member transition
journal ID**. The independent health collector currently refuses that switch
and keeps the old durable cursor, including any events it had not yet polled.
Preserve the old member directory, new seed approval, both journal snapshots
and collector files. The durable-transition API will remain partial with
`journal_changed_requires_reconciliation` until an authenticated handoff
between member generations is implemented and verified. Do not clear or
rename the collector journal merely to restore a green card; see the
[transition evidence contract](../contracts/ha-transition-durability.md#same-incarnation-member-reseed-is-a-separate-evidence-handoff).

On **frozen, private copies** of the approved seed files, stopped source
Primary, and replacement member, run the read-only origin checks before any
collector handoff:

```bash
halro cluster verify-seed-approval-snapshot --config TARGET_CONFIG --approved-files-dir APPROVED_FILES_DIR --manifest SEED_MANIFEST
halro cluster verify-seed-source-origin --source-config SOURCE_CONFIG --source-snapshot-dir SOURCE_SNAPSHOT_DIR --target-config TARGET_CONFIG --approved-files-dir APPROVED_FILES_DIR --manifest SEED_MANIFEST
halro cluster verify-member-seed-origin --config TARGET_CONFIG --snapshot-dir REPLACEMENT_SNAPSHOT_DIR --approved-files-dir APPROVED_FILES_DIR --manifest SEED_MANIFEST
halro-ha-health -preflight-reseed-handoff PRIVATE_PREFLIGHT_JSON
```

The private preflight JSON is version 1 and supplies `environment`, `cluster`,
the complete `members` list, frozen `collector` manifest path, `retired`,
`source`, and `replacement` objects (each with `config`, `snapshot_dir`, and
operator-recorded `frozen_at`), `approved_files_dir`, `seed_manifest`, and
`fencing_evidence_sha256`. The preflight requires the collector's old chain to
match the retired member's authenticated final head and checks the seed/source/
replacement relationship. Its `authenticated_inputs_match_handoff_uncommitted`
status does **not** install a new collector generation. To commit after backing
up the exact collector files, stop the health Deployment, confirm the collector
process has exited, and run against the exclusive collector PVC or a separate
restoration copy:

```bash
halro-ha-health -commit-reseed-handoff PRIVATE_PREFLIGHT_JSON -durable-transition-journal STOPPED_COLLECTOR_MANIFEST
halro-ha-health -verify-durable-snapshot STOPPED_COLLECTOR_MANIFEST -environment ENV -cluster CLUSTER -members NODE_0,NODE_1,NODE_2
```

The command rejects a collector whose files differ from the frozen preflight
copy. Its atomic version-3 manifest retains the old chain and records the new
baseline as a separate generation. Retain both frozen member directories and
the approved seed files; use a version-3 direct member snapshot manifest with
each generation's `node_id`, `incarnation`, `journal_id`, matching `config`, and
`snapshot_dir` to authenticate the entire retained chain after commit. Only
then restart the health service with a binary that understands collector format
3 and check that the new member resumes from its own baseline. The freeze times
and fencing digest are supplied provenance references, not independent fencing
attestation. Local verification does not provide immutable external retention
or authorize deleting either generation.

To account for events missed before the old member stopped, use a frozen copy
of its retired directory and a configuration that points at its Master Key but
whose `storage.data_dir` is **not** the copy being read. Take the old collector
cursor verbatim, then run `halro cluster verify-transition-snapshot` with
`--snapshot-dir`, `--after-journal-id`, `--after-sequence`, and `--after-digest`.
The command rejects a cursor from another journal or with a wrong digest and
returns at most 64 MAC-authenticated committed events. For further pages,
repeat with the returned `next_cursor` and require the same snapshot inventory
hash on every page. Keep the output with the frozen source bytes. To append
that verified tail, **stop the health collector**, retain a byte-for-byte
backup of its private journal and segments, then run `halro-ha-health` with
`-reconcile-retired-member-snapshot`, `-retired-member-config`,
`-durable-transition-journal`, and the exact `-environment`, `-cluster`,
`-members` inventory. The process takes the journal lock and reauthenticates
each page before appending. Record its output and verify a frozen copy of the
resulting collector. The reported status explicitly keeps the new member
journal unapproved; resume collection only after retaining the old source,
backup, output and newly verified collector bytes.

## Replica backup and reporting

Enable maintenance on a Replica, wait for readiness to fail, then create and
verify the backup while the serve process keeps liveness up but releases the
data lock:

```sh
halro cluster maintenance on --config /etc/halro/config.yaml
halro backup create --replica --config /etc/halro/config.yaml \
  --output /secure/replica.hmbk --key-file /secure/backup.key
halro backup verify --file /secure/replica.hmbk --key-file /secure/backup.key
halro cluster maintenance off --config /etc/halro/config.yaml
```

After the Replica catches up, stop the Primary and record the verified archive:

```sh
halro cluster report-backup --config /etc/halro/primary.yaml \
  --file /secure/replica.hmbk --key-file /secure/backup.key \
  --username admin --password-file /secure/admin-password
```

## Whole-cluster restore

Isolate all old members and client traffic. Restore a verified HA archive with
a new incarnation; the restored node is the initial Primary of term 1:

```sh
halro backup restore --config /etc/halro/config.yaml \
  --file /secure/replica.hmbk --key-file /secure/backup.key \
  --confirm-backup-id bkp_... --incarnation inc_new_...
```

Seed every Replica from that restored Primary. An old PVC retains the former
incarnation and is rejected; do not edit it into the new cluster.

## Leave HA

Stop the selected member and require exact cluster/member confirmation plus
administrator reauthentication:

```sh
halro cluster leave --config /etc/halro/config.yaml \
  --confirm production-a/halro-1 --username admin \
  --password-file /secure/admin-password
```

Remove the `replication` block only after `leave` succeeds. The command atomically
renames the authenticated member state to the private `cluster.left` retirement
tombstone; retain it until the change is accepted, then destroy it with the
retired PVC under the normal data-retirement procedure. Destroy or isolate every
other retained member directory so two Standalone copies cannot both hold live
Provider credentials.
