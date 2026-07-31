# Encrypted backup

Heimdall backups are offline, atomic, encrypted snapshots. Stop the server
before creating one; the command acquires the same exclusive data-directory
lock as the runtime and fails if another process owns the directory.

Create a dedicated random backup key. It is not the Heimdall Master Key:

```bash
umask 077
openssl rand 32 > backup.key
```

Create and verify a backup:

```bash
heimdall backup create \
  --config ./config.yaml \
  --output /secure-backups/heimdall-2026-07-31.hmbk \
  --key-file ./backup.key

heimdall backup verify \
  --file /secure-backups/heimdall-2026-07-31.hmbk \
  --key-file ./backup.key
```

The key file must be a regular file containing exactly 32 bytes and must not
be accessible by group or other users. Losing this key makes the backup
unrecoverable. Store it independently from both the backup and Heimdall Master
Key.

## Consistency boundary

The offline data lock is the backup barrier. Inside it Heimdall:

1. verifies the Audit checkpoint and records `backup.create`;
2. creates a transactionally consistent bbolt snapshot;
3. takes an fsynced committed-prefix Ledger snapshot while holding the Ledger
   writer lock and records its exact generation/offset/sequence;
4. validates the Usage checkpoint against its embedded watermark;
5. verifies the committed Parquet manifest against the Ledger aggregate;
6. includes only Parquet files pinned by that manifest;
7. writes the encrypted archive to a same-directory temporary file;
8. `fsync`s it, atomically publishes it without overwriting an existing name,
   and `fsync`s the output directory.

The package contains the configuration copy, metadata snapshot, fixed Ledger
snapshot (never the subsequently growing live WAL), Audit log, committed Usage
manifest and its referenced Parquet files. It does
not contain `master.key`. The manifest stores only its SHA-256 fingerprint so
a future restore can reject the wrong Master Key before switching data.

The snapshot primitive permits append callers to queue while the writer lock is
held. A deterministic 100-writer test verifies that the backup contains only
complete records at or before the returned watermark; restore replays exactly
that prefix, while writes committed afterward remain only in the pre-restore
rollback directory. The public CLI remains deliberately offline in v1, so this
concurrency property is defense in depth for the epoch boundary rather than an
online-backup promise.

## Encryption and verification

The archive uses a domain-separated key and chunked AES-256-GCM. Every chunk
authenticates its sequence, length and final-state flag. A dedicated
authenticated final record detects truncation; verification also rejects
trailing bytes, unsafe paths, duplicate entries, symlinks, hard-linked source
files, checksum mismatches and unsupported schema versions.

`backup verify` is read-only and never prints file contents or keys.

## Restore

Restore is offline and deliberately requires the verified Backup ID as a
destructive confirmation:

```bash
heimdall backup verify \
  --file /secure-backups/heimdall-2026-07-31.hmbk \
  --key-file ./backup.key

heimdall backup restore \
  --config ./config.yaml \
  --file /secure-backups/heimdall-2026-07-31.hmbk \
  --key-file ./backup.key \
  --confirm-backup-id bkp_0123456789abcdef0123456789abcdef
```

`heimdall restore` is an equivalent top-level alias with the same required
flags; `backup restore` remains available for command grouping compatibility.

The server must be stopped. Restore verifies the encrypted archive and exact
Master Key fingerprint, extracts only regular manifest-listed files into a
same-filesystem staging directory, opens/migrates bbolt, authenticates the
Vault check, replays WAL to the manifest watermark, verifies Audit and Parquet,
and appends a restore Audit event. It then holds locks on both live and staged
directories during an atomic directory rename.

The old live directory is preserved as the `previous_data_dir` returned in the
JSON result. Start and validate the restored server before deleting that
rollback directory. `storage.master_key_file` must be outside `storage.data_dir`
because the Master Key is intentionally never packaged in a backup.
