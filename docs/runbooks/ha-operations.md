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
