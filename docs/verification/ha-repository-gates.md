# HA repository verification gates

This file is the repository-side evidence index for
`docs/todo/halro-ha-architecture.zh-CN.md` §17. It deliberately separates
deterministic repository tests from the production admission evidence in §1.3
and from kind/Linux reference-host exercises.

## Repository gates

| Contract | Observable oracle | Automated evidence |
|---|---|---|
| Durable ordering and crash recovery | Native bytes never trail an acknowledged index; partial tails truncate, complete missing/corrupt suffixes fail closed, and confirmation never precedes state fsync | `TestOrderingJournal*`, `TestRecoverLocalCommits*`, `TestPrimaryCoordinatorPersists*`, `TestReplicaReceiverPersists*`, and the durability injection seams in `internal/replication` |
| Request lifecycle recovery | A committed intent is delivered on restart, idempotent create does not duplicate a record, and stale activation refuses traffic | `TestAnIntentLeftByACrashIsDeliveredOnTheNextStart`, `TestRetriedCreateDoesNotProduceASecondRecord`, `TestRecoveryReplaysEveryActivationDomain`, `TestStaleActivationRefusesDataPlaneTraffic` |
| Confirmed mutation survives member change | A mutation observed after quorum confirmation is present on the promoted Replica | `TestPrimaryAndReplicaRuntimesReplicateAndApplyAnAuditFrame` runs two real mTLS runtimes, revokes a Gateway Key, stops both members, promotes the Replica, and reopens its projection to prove the key remains revoked |
| At most one confirming authority | A higher-term durable promise demotes the old Primary in the same state publication; stale/wrong-term ACKs cannot advance confirmation | `TestPromiseDemotesPrimaryInTheSameDurablePublication`, `TestStatePublisherPersistsPromiseBeforePromotion`, `TestPrimaryCoordinatorDoesNotRecoverAvailabilityFromAStaleACK`, `TestReplicaReceiverRefusesGapWrongTermAndIndexFork` |
| Freshness and suffix reconciliation | Promotion compares `(last_frame_term,index)`; missing/different native bytes, wrong baselines, and interrupted Roll recovery fail closed | `TestValidatePromotionPromisesUsesTermThenIndexFreshness`, `TestRecoverLocalCommitsFailsClosedOnMissingOrDifferentSourceBytes`, `TestRecoverLocalCommitsRejectsSameCursorDifferentBaselineHistory`, `TestRecoverLocalCommitsCompletesNativeLedgerRollBeforeRequeue` |
| Replica has no local authoritative maintenance writes | Replica-native stores refuse ordinary append/update; Admin sessions are in memory; backup and login leave replicated bytes unchanged | `TestPrimaryAndReplicaRuntimesReplicateAndApplyAnAuditFrame`, `TestReplicaBackupIsReadOnlyAndCarriesOneAppliedPrefix`, every `replica_open_test.go`, and `TestMaintenanceHandlersExposeOnlyLivenessAndAuthenticatedModeMetric` |
| Provider objects | No final name exists before the final verified chunk; digest/range/path forgery fails; objects larger than one frame remain reconstructible | `TestProviderObjectMetadataRejectsTraversalAndRangeForgery`, `TestNativeSinkPublishesProviderObjectOnlyAfterFinalVerifiedChunk`, `TestPersistProviderObjectSourceAcceptsObjectsLargerThanOneFrame`, plus the mTLS runtime integration test |
| Seed publication | A target-bound MAC and every file/order digest are checked before one rename; a non-zero approved index is preserved; tamper leaves the target absent | `TestSeedApprovalAuthenticatesStagingBeforeAtomicReplicaPublication`, `TestSeedManifestDecoderRejectsTrailingJSON` |
| Replica backup, report, and restore | Backup does not mutate Replica authority; Primary verifies and records the report; HA restore requires a new incarnation | `TestReplicaBackupIsReadOnlyAndCarriesOneAppliedPrefix`, `TestReportReplicaBackupAuthenticatesArchiveAndRecordsPrimarySuffix` |
| Protocol and identity failures | Bounded decoders, frame MAC/digest, Master Key proof, mTLS 1.3, SPKI pin, and incompatible-version paths fail closed | `TestFrameRefusesCorruptionAndNonCanonicalLengths`, `TestAuthenticatedSessionRefusesWrongClusterKey`, `TestReplicationTLSRefusesCAValidWrongSPKIPin`, `TestPeerHelloRefusesIdentityReplayAndIncompatibleRanges` |
| Cluster CA rotation | A dual-root bundle accepts an old-cert client and new-cert server while each connection still requires its reviewed SPKI | `TestReplicationTLSDualCABundleSupportsOneCertificateAtATimeRotation` and `docs/runbooks/cluster-ca-rotation.md` |
| Schema boundary | Replica can open an older supported schema without migration and may advance applied only after validating a confirmed boundary | `TestOpenReplicaDoesNotMigrateOrRejectABehindSchema`, `TestReplicaApplierValidatesConfirmedSchemaBoundaryBeforeAdvancing` |
| Kubernetes and observability artifacts | HA topology/security invariants are structurally asserted; Prometheus rules parse and their scenarios evaluate | `deploy/kubernetes/manifests_test.go`, `deploy/observability/validate.sh` |

These tests use deterministic restart/fault seams plus a real TCP/mTLS
double-runtime integration test. They do not claim kernel ENOSPC behavior,
Kubernetes endpoint timing, network-partition behavior, or reference-host
performance.

## Target-environment gates

The following remain release/production evidence, not repository coding tasks:

- kind: Pod deletion, SIGTERM/SSE drain, PDB, maintenance sentinel, endpoint
  behavior, single-PVC re-seed, whole-cluster restore, and old-incarnation
  rejection;
- adjacent released binaries: the complete same-schema rolling-upgrade
  sequence. A real schema-boundary transition is not a v1 feature; §15 requires
  offline restore to a new incarnation for schema-changing releases;
- Linux reference host: real ENOSPC/read-only/slow-disk behavior and the frozen
  §16.3 Standalone-versus-three-member performance comparison;
- §1.3: G0–G7 production acceptance, the 72-hour soak, and measured incident
  RTO.

None of those gates may be marked passed by a local `go test` result.
