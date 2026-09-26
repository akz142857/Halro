# Audit integrity

Audit records are stored in `data/audit/audit.log` as append-only framed
records. Each frame has a monotonic sequence, the SHA-256 hash of the previous
frame, and an HMAC-SHA256 made with a key derived from the master key under the
independent `halro:audit:v1` HKDF domain. Audit events have a fixed schema
and do not accept arbitrary request bodies, credentials, Gateway keys, prompts,
or model responses.

The current Audit head (record count, byte offset, and frame hash) is anchored
in bbolt after each Audit append. Startup verifies the full HMAC/hash chain and
reconciles it with that checkpoint. This detects modification, insertion,
reordering, and deletion of a committed suffix when the metadata checkpoint
remains intact. A crash between the Audit `fsync` and checkpoint transaction
can leave the log one record ahead; startup verifies the record and advances
the checkpoint.

Verify offline while the server is stopped:

```text
halro audit verify --config ./config.yaml
```

The guarantee is tamper evidence, not non-repudiation. An attacker with root
control, the master key, and the ability to roll back both files and external
backups remains outside this boundary. Future backup manifests must pin the
Audit head to provide an external rollback anchor.

## HA cluster event schemas

Cluster lifecycle actions use the same bounded `audit.Event` envelope. Their
`metadata` object is fixed by action; it must not contain certificate bytes,
SPKI or Master Key fingerprints, peer addresses, credentials, object bytes, or
free-form operator input.

| Action | Writer | Required metadata |
|---|---|---|
| `cluster.promote` | new Primary, new term | `cluster_id`, `incarnation`, `node_id`, `term`, `expect_term`, `expect_index`, `old_primary`, bounded `fenced_by`, `promise_nodes`, `promise_digest`, `self`, `no_peer_promise` |
| `cluster.stepdown.requested` | new Primary, new term | `cluster_id`, `incarnation`, `from_node`, `to_node`, `term`, `expect_term`, `expect_index`, `leadership_index`, and promise summary |
| `cluster.stepdown.completed` | new Primary, new term | the same transfer identity and promise summary after the requested record |
| `cluster.leave` | stopped local member before state removal | `cluster_id`, `incarnation`, `node_id`, `role`, `term`, `applied_index` |
| `cluster.backup.reported` | Primary | `backup_id`, `source_node_id`, `applied_index`, manifest digest, verification result |

Promotion and stepdown are always-reverify actions. Failed authorization never
becomes a replicated Audit event because no new authority exists; it remains a
local security log. A successful promotion writes its event only after the
new term is durable, so the evidence cannot be truncated with the old term.

Offline seeding deliberately uses a versioned, cluster-key-MACed seed manifest
instead of appending Audit events. Appending on a stopped Primary would move the
approved snapshot beyond its confirmed index. The manifest binds source and
target node IDs, term/index, ordering head, projection, actor/time, and every
authoritative file digest; it is retained as the approval receipt.
