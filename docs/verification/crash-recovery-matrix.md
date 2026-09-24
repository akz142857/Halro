# Crash and Recovery Matrix

Date: 2026-08-11

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
| Usage checkpoint moves behind/ahead incorrectly | Monotonic watermark validation rejects or safely discards it | `TestCheckpointWatermarkRejectsAlreadyAggregatedLedgerPrefix`, `TestUsageCheckpointAheadOfLedgerHeadIsDiscarded`, `TestUsageCheckpointPersistenceAndMonotonicity` | Pass |
| Parquet partition is modified | Manifest verification rejects it | `TestExporterDetectsParquetTampering` | Pass |
| bbolt metadata is newer than this binary | Open rejects it without mutating the database | `TestMetadataNewerSchemaIsRejectedWithoutMutation` | Pass |
| Master Key rotation stops at any of nine snapshot/rewrite/DB publication/key publication/bridge cleanup boundaries | Rerunning with the same replacement key finishes the protocol; Credential plaintext and Audit chain remain exact, Admin sessions are invalidated, retired ciphertext/bridge bytes are absent after compacted cleanup | `TestMasterKeyRotationRecoversFromEveryPublicationKillPoint` | Pass |
| 100 Ledger appends overlap the backup snapshot | Snapshot contains an fsynced complete prefix only; archive verification and restore reproduce exactly the manifest watermark, while the rollback directory retains the full live suffix | `TestSnapshotIsExactDuringOneHundredConcurrentAppends`, `TestBackupRestoreMatchesManifestDuringOneHundredConcurrentLedgerWrites` | Pass |
| Reference host recovers a 10 GiB WAL | Startup verifies every frame, then full State replay reaches the exact final watermark; measured published bound is 68.578 seconds with 12.1 MB HeapAlloc for the near-1-MiB-frame profile | `TestTenGiBWALRecoveryProfile` (opt-in) | Pass with published bound |
| Process stops between a metadata journal frame's fsync and its bbolt commit | The recorded transaction is replayed at the next attach; the projection ends level with the journal | `TestACrashBetweenTheFrameAndTheCommitIsReplayed`, `TestReplayIsIdempotent` | Pass |
| Metadata journal's final frame is partial | Partial tail is truncated; the chain before it stays appendable and verifiable | `TestATornTailIsRepairedRatherThanRefused` | Pass |
| Metadata journal committed bytes are modified, or signed by another key | Verification fails and the instance refuses to start | `TestAFlippedByteIsCorruptionNotACrash`, `TestAnotherKeyCannotAppend`, `TestDoctorNamesADivergenceRatherThanRepairingIt` | Pass |
| bbolt applied sequence is ahead of the journal, or follows a different epoch | Open fails closed rather than choosing between discarding recorded writes and overwriting newer state | `TestAProjectionAheadOfItsJournalFailsClosed`, `TestAnEpochMismatchFailsClosed` | Pass |
| Metadata journal is missing on a database that recorded a position in one | A new epoch is published with that database as its starting projection | `TestAMissingJournalOnADatabaseThatFollowedOneFailsClosed` | Pass |
| A batched metadata callback fails beside its siblings | The failing caller alone is refused; its operations never reach the journal, and the survivors commit in a new transaction | `TestAFailingBatchSiblingDoesNotReachTheJournal` | Pass |
| Restore publishes a directory carrying an archived metadata journal | The archived chain is withdrawn and the next attach opens a fresh epoch | `TestRestoreWithdrawsTheArchivedJournalAndOpensANewEpoch` | Pass |
| A data directory written before the metadata journal existed is opened | Epoch 1 is published with the database as its starting projection and the key is sealed; no re-initialisation | `TestAnInstanceThatPredatesTheJournalStartsAndSealsItsOwnKey` | Pass |
| Process receives `SIGKILL` with accounting writes in flight | No residual data lock; `doctor` healthy; every request the process had reported settled is present after recovery | `TestSIGKILLLeavesEveryReportedRequestRecoverable` | Pass |
| Metadata journal frame cannot be made durable (ENOSPC on write, EIO on fsync) | The bbolt transaction is refused rather than committed, so no write exists that the journal does not describe | `TestAFullDiskRefusesTheWriteRatherThanCommittingItUnrecorded` | Pass |
| Metadata journal append fails partway through a workload | The chain does not advance, the file still verifies, and the writes before it survive reopen | `TestTheJournalIsIntactAfterARefusedFrame` | Pass |
| Metadata journal disk is full while node-derived state is written | Checkpoints and route suspensions still commit; they are not journalled and must not depend on it | `TestNodeDerivedWritesSurviveAFullJournalDisk` | Pass |
| Data directory denies writes | Initialization and `Open` refuse; `doctor` still reads it, because a read-only filesystem is when an operator runs it | `TestInitializeRefusesAnUnwritableParent`, `TestOpeningAnUnwritableDataDirectoryFailsClosed`, `TestDoctorStillReadsAnUnwritableDataDirectory` | Pass |
| Ledger *directory* denies writes while the WAL file is already open | Appends continue — a directory's write bit governs entries, not open files — and only new-file operations fail. Recorded so a real EROFS mount is not mistaken for covered | `TestAnUnwritableLedgerDirectoryStillAppends` | Pass, with the gap named |
| Backup is truncated/tampered | AEAD final record/checksum verification rejects it | `TestEncryptedBackupRejectsTamperAndTruncation` | Pass |
| Restore confirmation is wrong | Live directory remains untouched | `TestRestoreValidatesStagesAtomicallyAndPreservesRollbackDirectory` | Pass |
| Restore succeeds | Staged Vault/WAL/Audit/Usage validate, atomic switch succeeds, old directory remains | `TestRestoreValidatesStagesAtomicallyAndPreservesRollbackDirectory` | Pass |

Operator recovery policy:

- never edit WAL, Audit, bbolt, or Parquet in place;
- stop the server and retain a byte-for-byte copy before any recovery command;
- prefer restoring a verified encrypted backup to truncating committed data;
- after restore, run configuration, Audit, Usage, and readiness checks before deleting `previous_data_dir`;
- a checksum failure in committed data is not treated as an automatically repairable tail.
