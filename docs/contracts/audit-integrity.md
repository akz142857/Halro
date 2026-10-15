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
