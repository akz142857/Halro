# File-mode Master Key rotation

Owner: Security and SRE  
Applies to: `storage.master_key.mode: file`

This is an offline, copy-on-write rotation. It changes the Master Key, rewrites
all Vault ciphertext, invalidates Admin sessions, and atomically publishes the
new metadata database and key file. Do not use this procedure for `key_slots`;
follow the [M11 KMS key lifecycle runbook](m11-kms-key-lifecycle.md) instead.

## Preconditions

1. Confirm `storage.master_key.mode` is `file`, the configured key is outside
   `storage.data_dir`, and the data directory parent is writable.
2. Create and verify a fresh encrypted backup. Record its backup ID, schema
   version, Master Key fingerprint, and the old key generation that it needs.
3. Stop Halro and prove the Gateway and Admin listeners are no longer serving.
4. Retain the old Master Key under the approved key-custody and backup-retention
   policy. A pre-rotation backup cannot be restored using only the new key.
5. Create a separate replacement key; never overwrite the configured key by
   hand:

   ```bash
   umask 077
   openssl rand 32 > /secure/path/new-master.key
   test "$(wc -c < /secure/path/new-master.key | tr -d ' ')" = 32
   test "$(stat -f '%Lp' /secure/path/new-master.key 2>/dev/null || stat -c '%a' /secure/path/new-master.key)" = 600
   ```

## Rotate

Run the command once and preserve its secret-free JSON output:

```bash
./halro key rotate --config /etc/halro/config.yaml \
  --new-key-file /secure/path/new-master.key
```

The expected result identifies the old and new SHA-256 fingerprint, rewritten
record count, and completed key version. It must not print key bytes. Do not
change `storage.master_key.file` before the command reports success; successful
rotation atomically publishes the new configured key itself.

## Interruption recovery

If the host or command stops, do not restore individual files, edit bbolt, use
a different replacement key, or start a second rotation. Keep Halro stopped and
rerun the exact command with the same `--new-key-file`. The authenticated
temporary bridge is designed to recover both `new DB / old key` and
`new DB / new key` interruption states. Escalate if the retry rejects that same
key; preserve the live directory, temporary files, old key, new key, and logs.

## Validate and close

1. Run `halro doctor --config /etc/halro/config.yaml`; require the Vault key
   check, Audit chain, Ledger chain, schema, and data lock checks to pass.
2. Start Halro. Require readiness, an Administrator login with fresh MFA, one
   authorized Gateway request, and a matching Ledger/Usage record.
3. Create and verify a post-rotation backup. Record that it needs the new
   fingerprint. Keep the old key for every retained pre-rotation backup.
4. In an isolated restore drill, verify that a pre-rotation archive is rejected
   by the new key with both fingerprint prefixes, then configure its recorded
   old Master Key and prove the restore succeeds. Never test this on live data.
5. Archive the before/after fingerprints, backup IDs, Audit event IDs, doctor
   output, readiness evidence, and rollback disposition without key material.

Rollback means restoring the complete pre-rotation backup with its recorded old
Master Key while Halro is stopped. Never pair an old database with a new key, a
new database with an old key, or copy only `metadata.db`.
