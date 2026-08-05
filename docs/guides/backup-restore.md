# Encrypted backup

Heimdall backups are offline, atomic, encrypted snapshots. Stop the server
before creating one; the command acquires the same exclusive data-directory
lock as the runtime and fails if another process owns the directory.

## What must survive a deployment

Heimdall is a single-writer service. Replacing a binary, image, container, or
Pod is safe only when the following state survives independently of that
workload:

- the complete `storage.data_dir`, including bbolt metadata, Ledger WAL, Audit,
  committed Usage files, and locally retained Provider objects;
- in File mode, the exact `storage.master_key.file`, stored outside the data
  directory;
- configuration and any referenced TLS or Metrics credential files.

The data directory is one consistency unit. Never restore only `heimdall.db`
or combine a database, WAL, Audit log, Usage tree, or Provider objects from
different snapshots. Configuration and credential mounts are desired state,
not substitutes for a data backup.

The encrypted archive contains the configuration copy, metadata snapshot,
fixed Ledger snapshot, Audit log, committed Usage manifest and its referenced
Parquet files, and every local Provider object referenced by the metadata
snapshot. A missing or unsafe referenced Provider object makes backup creation
fail closed. Uncommitted Usage files and orphan Provider-object temporary files
are not included.

The archive deliberately does **not** contain `master.key`, the backup key,
TLS private keys, Metrics bearer tokens, cloud workload-identity tokens, or
Recovery credentials.

> [!IMPORTANT]
> Keep three independently recoverable items: the encrypted `.hmbk` archive,
> its 32-byte backup key, and the matching File-mode Master Key (or the KMS
> permissions and keys needed by the archived Key Slot descriptor). Losing any
> one of them can make the backup unusable.

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

The output path must be absolute, outside `storage.data_dir`, and must not
already exist. Write it to a dedicated backup filesystem or upload the verified
archive to an immutable/versioned object store. Do not store the backup key in
the same bucket, volume, snapshot, or failure domain as the archive.

## Repository backup helper

For a source checkout, stop Heimdall and run:

```bash
make backup
```

The target builds `bin/heimdall` when necessary and calls
`scripts/backup.sh`. The helper:

1. creates a 32-byte `backup.key` with mode `0600` if it is absent;
2. runs offline `doctor`;
3. writes a UTC-timestamped archive under `backups/`;
4. immediately runs `backup verify` and prints its authenticated manifest.

Override every location without editing the script:

```bash
make backup \
  CONFIG=/etc/heimdall/config.yaml \
  BACKUP_DIR=/secure-backups \
  BACKUP_KEY_FILE=/secure-secrets/heimdall-backup.key \
  BACKUP_NAME=heimdall-before-v1.1.0
```

Both the default key and archives are ignored by Git, but Git ignore is not
key custody. The defaults are intended for local operation. Production must
store the key independently from the archive and Master Key. Reusing one
Backup Key is convenient but increases compromise scope; use a different
`BACKUP_KEY_FILE` for each rotation period when policy requires separation.

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

The manifest stores only the Master Key SHA-256 fingerprint so a future restore
can reject the wrong Master Key before switching data.

Format v2 also records the WAL feature epoch/minimum reader, immutable pricing
state digest, pending pricing/adjustment intent digest and count, and both
Settlement and Adjustment Parquet watermarks. Restore rejects a cross-store
mixture before publication. Metadata schema v14 includes inert pricing
Proposals in the pricing snapshot digest; Proposal presence never changes the
Gateway selection path.

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

Verification proves that the encrypted archive is authentic and complete. It
does not prove that the operator still controls the matching Master Key/KMS
identity or that the target environment can complete a restore. Perform
periodic restore drills in an isolated environment and retain the non-secret
Backup ID, manifest, binary version, result, and date as operational evidence.

## Upgrade procedure

Use this sequence for every binary or container-image upgrade:

1. retain the current binary/image digest and configuration;
2. stop Heimdall and confirm that it released the data-directory lock;
3. run `heimdall doctor --config ...` with the current binary;
4. create and verify a new encrypted backup;
5. run the new binary's `config check` against a copy of the configuration;
6. start exactly one new instance against the unchanged persistent state;
7. validate liveness, readiness, Admin system status, Metrics, one normal
   request, one streaming request, and Usage settlement.

Do not downgrade a data directory that a newer binary has migrated. Stop the
new binary and restore the pre-upgrade archive instead.

## Docker and Kubernetes storage layout

Mount a persistent **parent** directory and configure `storage.data_dir` as a
child of it:

```yaml
storage:
  data_dir: /var/lib/heimdall-volume/data
  metadata_file: heimdall.db
  master_key:
    mode: file
    file: /run/secrets/heimdall-master.key
```

```yaml
# Docker Compose fragment
services:
  heimdall:
    volumes:
      - heimdall-state:/var/lib/heimdall-volume
      - ./config.yaml:/etc/heimdall/config.yaml:ro
      - ./secrets/master.key:/run/secrets/heimdall-master.key:ro

volumes:
  heimdall-state:
```

Do not set the volume mount point itself as `storage.data_dir`. Restore stages
and atomically renames the complete live directory within its parent; a mounted
directory cannot generally be renamed. The parent must be writable and the
staging and live directories must be on the same filesystem.

For Kubernetes, use a retained `ReadWriteOnce` PVC, mount it at
`/var/lib/heimdall-volume`, and keep `replicas: 1` with `strategy.type:
Recreate`. Do not configure an HPA or allow two Pods to write the same data
directory. `emptyDir` is suitable for `/tmp`, never for Heimdall state.

```yaml
spec:
  replicas: 1
  strategy:
    type: Recreate
  template:
    spec:
      containers:
        - name: heimdall
          volumeMounts:
            - name: state
              mountPath: /var/lib/heimdall-volume
      volumes:
        - name: state
          persistentVolumeClaim:
            claimName: heimdall-state
```

Kubernetes Secrets survive Pod replacement but are not disaster-recovery
copies: namespace deletion or control-plane loss can remove them. File-mode
initialization and key rotation also require atomic writes, while projected
Secret volumes are read-only. Use a separately protected writable key store
with an independent export/escrow procedure, or use the reviewed Key Slot/KMS
deployment model. Never initialize a new Master Key when attaching an existing
data PVC.

An offline Kubernetes backup workflow must scale/stop the Deployment, run a
one-shot Job that mounts the same PVC plus separate backup-key and output
locations, execute `backup create` followed by `backup verify`, publish the
verified archive, and then restore the Deployment. A CronJob racing the live
Pod will fail to acquire the exclusive lock and is not an online-backup design.

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
rollback directory. In File mode, `storage.master_key.file` must be outside `storage.data_dir`
because the Master Key is intentionally never packaged in a backup.

After restore, run the following before accepting traffic:

```bash
heimdall doctor --config ./config.yaml
```

Then validate liveness, readiness, Audit, Admin authentication, Metrics,
Provider connectivity, normal and streaming requests, and Usage settlement.
Restored Admin sessions and pending MFA challenges are invalidated. Retain
`previous_data_dir` until validation and the rollback retention window finish.

If a Price Version was scheduled after the backup timestamp but is already due
at restore time, the affected Deployment enters pricing quarantine. The old
scheduled version is not allowed to resume traffic automatically. In Admin,
review its provenance and the Provider's current terms, then perform the
recently re-authenticated restore confirmation or create a valid successor.
The confirmation is appended to Audit.

## Retention and restore drills

- create and verify a backup before every upgrade, migration, Master Key
  rotation, or destructive offline operation;
- keep multiple generations under a documented daily/weekly/monthly policy;
- monitor archive age, verification status, storage capacity, and upload
  failures;
- test restoration regularly with the exact release artifact and target
  identity; archive verification alone is insufficient;
- record which Master Key generation or KMS descriptor each historical backup
  needs, and never retire that key while a retained backup depends on it;
- do not delete the source PVC, old data directory, or previous image until the
  new instance and its backup have passed validation.

# AWS KMS / Key Slot 恢复说明

Key Slot 模式的 manifest 额外记录 `key_slot_descriptor_sha256`。`backup verify` 只验证外层认证和 manifest checksum，`restore_drill_verified` 固定为 `false`；只有在目标 Workload Identity 下完成 staging descriptor/allowlist、KMS unwrap、Vault Key Check 和全部域验证，才能声明可恢复。

Primary 与 Recovery 的完整操作顺序、CloudTrail 副作用和 break-glass 撤权要求见 [M11 AWS KMS 灾备 Runbook](../runbooks/m11-kms-disaster-recovery.md)。
