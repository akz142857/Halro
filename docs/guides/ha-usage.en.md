# Halro HA Usage Guide

This guide is for operators deploying and running a three-member Primary/Replica cluster. It covers initial setup, status checks, manual handoff, recovery, and validation. For the exact offline file procedures, use the [HA operations runbook](../runbooks/ha-operations.md). The [HA test guide (Chinese)](../verification/ha-test-guide.zh-CN.md) defines the fault-drill sequence and pass criteria. [简体中文版](ha-usage.zh-CN.md).

> **Current status:** The repository implements the HA commands, replication runtime, and read-only Admin status page. Production admission, G0–G7, Linux/kind fault injection, a 72-hour soak, and RTO/RPO acceptance in the target environment remain incomplete. A local three-process exercise is not production approval. The `v0.8.5` Docker image shown in the README does not include `halro cluster`; confirm that your binary was built from source containing HA or from a later, accepted release artifact. Automatic failover is outside the current design.

## 1. How the cluster works

- Use three members, each with one process and its own data directory or PVC. At most one member is Primary at a time. Two members can replicate and produce backups, but after losing one member they cannot confirm writes that require a Replica; a two-member setup does not provide full write availability after failover.
- All members use the same `replication.cluster_id`, one shared `incarnation`, and the same Master Key material. Each member has a unique `node_id`, its own mTLS certificate and listen address, and SPKI pins for its peers. Only cluster members should reach the replication port.
- The Primary handles Gateway traffic and the normal Admin Console. A Replica is not a write endpoint: a client request routed there receives `503 not_primary`. Design client routing and retries for this topology.
- Some accounting and revocation writes wait for Replica confirmation and fail closed when the required Replica is unavailable. `durable_index`, `confirmed_index`, and `applied_index` describe different stages. A connected peer has not necessarily caught up.
- A restart, member failure, or network partition does not elect a Primary automatically. Use `stepdown` for a planned handoff. For an unplanned promotion, first fence the old Primary outside Halro, then run `promote` manually.

## 2. Prepare the binary, configuration, and data

From the repository root, build the current source for an **isolated test environment** and check that the CLI includes HA:

```sh
go build -trimpath -o ./bin/halro ./cmd/halro
./bin/halro version
./bin/halro help cluster
```

If you get `unknown command "cluster"`, check which binary you actually ran and where it came from. An old `bin/halro` is not replaced merely by updating Git. A production artifact still needs the separate release and acceptance gates.
The commands below use `./bin/halro` from the repository root; replace it with the installed binary path on each host.

First prepare a working Standalone data directory with an administrator using the [normal installation and initialization procedure](operator-guide.md), then stop that process. Start each member's complete configuration from [`configs/config.example.yaml`](../../configs/config.example.yaml); the fragment below is not a complete configuration file. Add a `replication` block to each member's configuration. This is an example for `halro-0`; change `node_id` and `peers` for the other members:

```yaml
replication:
  cluster_id: production-a
  node_id: halro-0
  listen: 0.0.0.0:9910
  peers:
    - name: halro-1
      address: halro-1.halro-internal:9910
      spki_sha256: "sha256:<64 lowercase hex digits of halro-1 certificate SPKI>"
    - name: halro-2
      address: halro-2.halro-internal:9910
      spki_sha256: "sha256:<64 lowercase hex digits of halro-2 certificate SPKI>"
  tls:
    ca_file: /run/secrets/halro-cluster/ca.crt
    cert_file: /run/secrets/halro-cluster/tls.crt
    key_file: /run/secrets/halro-cluster/tls.key
```

Replace the example addresses and SPKI values. Give each member a distinct `storage.data_dir` and nonconflicting Gateway, Admin, Metrics, and replication listen addresses, especially when testing on one host. Never let two processes share a data directory or writable PVC. Validate all three configurations:

```sh
./bin/halro config check --config /etc/halro/halro-0.yaml
./bin/halro config check --config /etc/halro/halro-1.yaml
./bin/halro config check --config /etc/halro/halro-2.yaml
```

The Kubernetes [`halro-ha-statefulset.yaml`](../../deploy/kubernetes/halro-ha-statefulset.yaml) is a topology example. Review and replace its image digest, Secrets, network policies, storage, and client entry point for your environment. Do not turn a Standalone Deployment into HA simply by setting `replicas: 3`.

## 3. Establish the Primary and seed two Replicas

**Every offline command requires the local member named by `--config` to be stopped and its data-directory lock released.** `cluster status` is read-only and can inspect a running member. Do not delete `.halro.lock` to bypass a running process.

1. Stop the existing `halro-0` process. Confirm its ordinary data directory, administrator, and Master Key are ready. Establish the initial Primary and record the chosen `incarnation`:

   ```sh
   ./bin/halro cluster establish --config /etc/halro/halro-0.yaml \
     --role primary --incarnation inc_example_001 --term 1
   ```

2. Before seeding a Replica, let the Primary's confirmed prefix catch up and **stop the Primary**. On the Primary, approve the target using an administrator account. If MFA is enabled, also provide `--totp-file` containing the current code:

   ```sh
   ./bin/halro cluster seed-approve --config /etc/halro/halro-0.yaml \
     --target halro-1 --output /secure/halro-1.seed.json \
     --username admin --password-file /secure/admin-password
   ```

3. Transfer the stopped Primary's authoritative snapshot over an authenticated, encrypted channel into a **private staging directory next to** the target `storage.data_dir`. The target's final data directory must not exist. Include `cluster/ordering.journal` and `cluster/provider-object-sources/`; exclude the source member's `cluster/state.json`, `cluster/maintenance`, `cluster/totp-watermark`, and `.halro.lock`. The manifest provides approval and integrity checks, not confidentiality. For two separate hosts, this example creates a new staging directory and uses SSH transport:

   ```sh
   # On halro-1: staging is a sibling of the target /var/lib/halro/data.
   mkdir -m 0700 /var/lib/halro/.seed-staging
   # On halro-0: the Primary is stopped; SSH identity and target permissions are set.
   rsync -a \
     --exclude='/.halro.lock' \
     --exclude='/cluster/state.json' \
     --exclude='/cluster/maintenance' \
     --exclude='/cluster/totp-watermark' \
     /var/lib/halro/data/ halro-1:/var/lib/halro/.seed-staging/
   ```

   Transfer the approved manifest to `/secure/halro-1.seed.json` on the target through a controlled channel. Replace the paths, transport identity, and permissions for your environment. The installer checks the files and ordering chain; see the [seed runbook](../runbooks/ha-operations.md#seed-a-replica) for failure handling.

4. Install the same Master Key material and the target's own certificate. With the **target process stopped**, install the seed:

   ```sh
   ./bin/halro cluster seed-install --config /etc/halro/halro-1.yaml \
     --staging /var/lib/halro/.seed-staging \
     --manifest /secure/halro-1.seed.json
   ```

5. Repeat the approval, transfer, and installation for `halro-2` using its own configuration and manifest. Then start the three members. Verify one Primary and two Replicas with matching `cluster_id` and `incarnation`. `cluster establish --role replica` is refused; a Replica must enter through an approved seed.

Replace the example `incarnation`, paths, and host names with your own values. Keep the administrator password file in a controlled private location; never put the password in command arguments, logs, or documentation.

## 4. Inspect status and decide whether a member can take over

Sign in to the current Primary's Admin Console and open **Cluster status** at `/admin/cluster`. The page refreshes every 10 seconds and has a manual refresh button. It shows the **local member's** role, cluster and node IDs, incarnation, term, promised term, startup readiness, three replication indexes, metadata projection, and authenticated data-session connectivity to each configured peer. It is read-only; it cannot initiate a handoff.

The authenticated `GET /admin/api/v1/cluster/status` endpoint provides the same local view. A Replica's Admin API can separately serve its own status, while the normal Console is served by the Primary. Neither UI nor API gives the peer's actual watermarks. Before a handoff or promotion, read authenticated status **on every member**:

```sh
./bin/halro cluster status --config /etc/halro/halro-0.yaml
./bin/halro cluster status --config /etc/halro/halro-1.yaml
./bin/halro cluster status --config /etc/halro/halro-2.yaml
```

Record each member's `role`, `term`, `promised_term`, `durable_index`, `confirmed_index`, and `applied_index`. In a stable cluster there should be exactly one Primary, each member should satisfy `durable >= confirmed >= applied`, and the Replicas should eventually catch up to the Primary's confirmed index. A candidate Replica must have **equal** values for its three indexes and meet the authenticated-prefix rules for the operation. An unreadable status is unknown, not index zero or a healthy member.

## 5. Planned manual handoff: `stepdown`

Plan client routing, long-running requests, and recovery for the maintenance window. The term and index below are **command examples**; replace them with values just read from the target member.

1. Keep the old Primary `halro-0` reachable on the replication channel and let target Replica `halro-1` catch up. Record status on all three members and confirm the target has `durable == confirmed == applied`.
2. **Stop `halro-1`'s Halro serve process** and wait for its data-directory lock to be released. Keep the old Primary running. On the target host, use the target's own configuration:

   ```sh
   ./bin/halro cluster stepdown --config /etc/halro/halro-1.yaml \
     --from halro-0 --to halro-1 \
     --expect-term 7 --expect-index 10241 \
     --username admin --password-file /secure/admin-password
   ```

3. The old Primary withdraws readiness and waits for admitted HTTP handlers to finish before freezing the prefix, durably promising the higher term, and exiting. If the index advances during this drain, the stale `--expect-index` makes the command fail. Restart the target as a Replica to let it catch up, stop it again, inspect its new status, and retry with the new values. **A refusal is not a completed handoff.**
4. After the command succeeds, start the new Primary `halro-1` and wait for `/health/ready` to return 200. Then start the former `halro-0` as a Replica. Check role, term, all three indexes, and client access on every member. There should be only one serving Primary, and the Replicas should catch up. If the former Primary refuses its metadata projection check, investigate or perform a new approved seed. Never edit `cluster/state.json` to force it online.

`promotion requires the local Halro process to be stopped: data directory is already locked by another process` means the target process still owns its directory. Stop the member named by `--config` and wait for it to exit. Do not stop the wrong Primary or delete the lock file.

## 6. Unplanned promotion and recovery

If the old Primary fails, first prove that it is fenced **outside Halro** so it cannot continue sending requests to a Provider. The accepted fencing assertions are `pod-deleted-pvc-retained` (the Pod was deleted and its PVC cannot be automatically reattached) and `node-isolated` (the node is disconnected or powered off). Merely killing a process is not sufficient fencing. Do not promote without evidence.

Choose the stopped, fully caught-up Replica with the most recent authenticated prefix. Run the command with that member's configuration. These term and index values are **examples** and must come from a fresh status read on the candidate:

```sh
./bin/halro cluster promote --config /etc/halro/halro-1.yaml \
  --expect-term 7 --expect-index 10241 \
  --old-primary halro-0 --old-primary-fenced-by node-isolated \
  --username admin --password-file /secure/admin-password
```

A three-member promotion also needs a peer's durable promise. A lagging candidate or one that cannot obtain the required promise must be refused; do not bypass the check. `--no-peer-promise` is only for the special two-member procedure, and writes requiring a Replica cannot be confirmed until another member returns. Evaluate `promote --self` under the runbook only when a stopped former Primary is waiting for an operator after startup adjudication. Once the new Primary is ready, confirm there is exactly one Primary before restoring client traffic. When an old member returns, check its term, incarnation, and projection. A Replica that lost its PVC needs a new approved seed. Never edit or copy `cluster/state.json` to "repair" a role.

## 7. Backup, restore, and upgrade

- **Replica backup:** Run `cluster maintenance on` on the Replica and confirm that readiness fails while liveness stays healthy. Create an encrypted archive with `backup create --replica` and run `backup verify`. Run `maintenance off` and wait for catch-up. Then stop the Primary and record the verified archive with `cluster report-backup`. See the [runbook](../runbooks/ha-operations.md#replica-backup-and-reporting) for complete commands and file permissions.
- **Whole-cluster restore:** Isolate all old members and client traffic. Restore a verified HA backup with a **new incarnation**, then seed every Replica. An old PVC retains the former incarnation and cannot simply rejoin; follow the [restore procedure](../runbooks/ha-operations.md#whole-cluster-restore).
- **Upgrade:** Change one member at a time. Confirm it is not the active Primary; perform a `stepdown` first if necessary. Validate adjacent-version and schema compatibility using the [HA test guide](../verification/ha-test-guide.zh-CN.md). The example StatefulSet uses `OnDelete`; it does not choose the upgrade order for you.

## 8. Validation checklist and common misreadings

Three processes on one host can exercise configuration, mTLS, replication, the Admin status page, and planned handoff. They cannot replace tests with independent nodes, PVCs, network partitions, and Linux storage failures. Use synthetic requests and a mock Provider first. For each step, save every member's `cluster status`, readiness, replication metrics, and Audit/Ledger checks. Follow the [HA test guide](../verification/ha-test-guide.zh-CN.md) for normal replication, one-Replica loss, handoff with long requests, stale-index refusal, promotion after fencing, asymmetric partitions, PVC loss, whole-cluster restore, adjacent versions, ENOSPC/slow disks, and a 72-hour soak.

| Observation | Interpretation and action |
| --- | --- |
| `unknown command "cluster"` | The binary is too old or the path points to an old artifact. Check `version`, `help cluster`, and the executable actually launched. |
| Data directory is locked | The **local** member named by the offline command is still running. Stop it and wait for the lock to be released. |
| A peer shows “connected” | Only an authenticated session is known. Read the three indexes on that peer to establish catch-up. |
| `--expect-term` or `--expect-index` differs | State changed since inspection. Read and evaluate it again; do not blindly increment or override a value. |
| `503 not_primary` | The request reached a Replica. Route to the current Primary or retry safely. |
| `503 replication_unavailable` | A write requiring Replica confirmation cannot proceed. Check quorum, network, and durable indexes; do not silently fall back to single-writer confirmation. |
| No ready Primary | Follow startup adjudication, fencing, and the runbook. The cluster will not elect one automatically. |

The current production-admission status and evidence requirements are recorded in the [HA architecture (Chinese), §§1.3 and 18](../todo/halro-ha-architecture.zh-CN.md). One successful local handoff proves only that exercise; it does not establish automatic failover, production RPO/RTO, or availability in the target environment.
