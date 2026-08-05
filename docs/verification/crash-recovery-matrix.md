# Crash and Recovery Matrix

Date: 2026-07-31

The matrix is exercised by package and integration tests, not by fault injection in a production host.

| Failure point | Required outcome | Test evidence | Result |
|---|---|---|---|
| Process stops after reservation, before settlement | Pending reservation survives reopen and remains authoritative | `TestPendingReservationSurvivesReopen` | Pass |
| WAL stops at every byte offset of a representative multi-record file | Only incomplete tail is truncated; committed prefix replays, sequence and final file watermark remain exact | `TestCrashRecoveryAcrossEveryByteTruncationPoint` | Pass |
| 10,000 deterministic random WAL crash cuts | Every complete frame replays through CRC/JSON/State with monotonic sequence and no duplicate Event ID | `TestTenThousandRandomCrashInjectionsRecoverCompleteRecordsWithoutDuplicateEventIDs` | Pass |
| WAL write returns ENOSPC or partial-write EIO | Accounting becomes Unavailable, queued/future appends perform no more disk I/O, partial tail repairs on restart | `TestWriteAndSyncFailuresMakeAccountingUnavailable` | Pass |
| WAL fsync returns EIO | Accounting becomes Unavailable; current/future Gateway requests return 503 before Provider invocation | `TestDurabilityFailurePreventsCurrentAndFutureProviderCalls` | Pass |
| WAL committed bytes are modified | Startup refuses silent repair and requires recovery | `TestChecksumCorruptionRequiresRecovery` | Pass |
| Audit final record is partial | Partial tail is truncated; valid chain remains | `TestOpenTruncatesOnlyPartialAuditTail` | Pass |
| Audit committed record/key is modified | Verification fails | `TestAuditDetectsTamperingAndWrongKey` | Pass |
| Usage checkpoint is absent | Aggregate rebuild from Ledger is identical | `TestDeletingUsageCheckpointRebuildsIdenticalAggregateFromLedger` | Pass |
| Process stops immediately before/after any of 126 checkpoint boundaries | Restored checkpoint plus Ledger suffix exactly equals full replay for snapshot and metrics | `TestCheckpointRecoveryMatchesFullReplayAcrossOneHundredKillPoints` | Pass |
| Usage checkpoint moves behind/ahead incorrectly | Monotonic watermark validation rejects it | checkpoint/store tests | Pass |
| Parquet partition is modified | Manifest verification rejects it | `TestExporterDetectsParquetTampering` | Pass |
| bbolt V2→V3 migration stops at any of 39 deterministic mutation/commit boundaries | The transaction leaves schema V2, all eight legacy routes, and the absence of the deployments bucket unchanged; a later open retries fully to V3 | `TestDeploymentMigrationSurvivesEveryInjectedKillPoint` | Pass |
| Master Key rotation stops at any of nine snapshot/rewrite/DB publication/key publication/bridge cleanup boundaries | Rerunning with the same replacement key finishes the protocol; Credential plaintext and Audit chain remain exact, Admin sessions are invalidated, retired ciphertext/bridge bytes are absent after compacted cleanup | `TestMasterKeyRotationRecoversFromEveryPublicationKillPoint` | Pass |
| 100 Ledger appends overlap the backup snapshot | Snapshot contains an fsynced complete prefix only; archive verification and restore reproduce exactly the manifest watermark, while the rollback directory retains the full live suffix | `TestSnapshotIsExactDuringOneHundredConcurrentAppends`, `TestBackupRestoreMatchesManifestDuringOneHundredConcurrentLedgerWrites` | Pass |
| Reference host recovers a 10 GiB WAL | Startup verifies every frame, then full State replay reaches the exact final watermark; measured published bound is 68.578 seconds with 12.1 MB HeapAlloc for the near-1-MiB-frame profile | `TestTenGiBWALRecoveryProfile` (opt-in) | Pass with published bound |
| Backup is truncated/tampered | AEAD final record/checksum verification rejects it | `TestEncryptedBackupRejectsTamperAndTruncation` | Pass |
| Restore confirmation is wrong | Live directory remains untouched | restore integration test | Pass |
| Restore succeeds | Staged Vault/WAL/Audit/Usage validate, atomic switch succeeds, old directory remains | `TestRestoreValidatesStagesAtomicallyAndPreservesRollbackDirectory` | Pass |

Operator recovery policy:

- never edit WAL, Audit, bbolt, or Parquet in place;
- stop the server and retain a byte-for-byte copy before any recovery command;
- prefer restoring a verified encrypted backup to truncating committed data;
- after restore, run configuration, Audit, Usage, and readiness checks before deleting `previous_data_dir`;
- a checksum failure in committed data is not treated as an automatically repairable tail.
