# Security role — E1/E2 evidence survey

- **Candidate SHA**: `f09ed2d768bf2cd335478223beb4d57af326013b`
- **Plan**: `/Users/ziy/Code/ClayCosmos/Halro/docs/verification/production-validation-plan.zh-CN.md`
- **Scope**: G3 in full (§138-149); security half of G1 (§116) and G4 (§155/§159/§162).
- **Method**: read-only source survey plus narrow `go test -run <Name> -count=1` re-runs of the
  load-bearing tests. No repo mutation, no `data/`, no `master.key`, no network, no full gate.
- **Legend**: `E2-covered` = an in-repo automated test fails if the control is removed.
  `E1-only` = design or documentation exists, no automated test. `REQUIRES-E4` = only
  demonstrable in the target environment.

> **Scope correction that governs everything below.** Plan §140 delegates G3 to
> `/Users/ziy/Code/ClayCosmos/Halro/docs/observability/admission-checklist.md`, whose rows are
> about the **Prometheus/Alertmanager Core** (`admission-checklist.md:13`), not about Halro's own
> data plane. Four of G3's six items therefore split into a Halro half that is testable in-repo
> and a Core half with no Halro code at all. Reporting a Halro-half PASS as the whole item is the
> easiest way to sign off G3 wrongly.

---

## 1. G3 item by item

### (a) mTLS identity: valid / missing / wrong / expired / revoked, plus rotation and rollback

**Scope finding first: mTLS exists on the `/metrics` listener only.** The only
`RequireAndVerifyClientCert` in the tree is
`/Users/ziy/Code/ClayCosmos/Halro/internal/app/tlsreload.go:299-300`, inside
`metricsTLSHolder.reload`. The Gateway data plane and the Admin plane are **not** mTLS-protected;
they authenticate with Gateway Keys (`internal/auth`) and admin sessions (`internal/adminauth`).
`/Users/ziy/Code/ClayCosmos/Halro/internal/app/runtime.go:1355` states outright that this path is
not covered by listener-level controls (firewall, WAF, mTLS, IP allowlists).

| Sub-case | Enforcing code | Automated test | Level |
|---|---|---|---|
| Valid client succeeds | `internal/app/tlsreload.go:296-301` | `TestMetricsTLSConfigRequiresAndVerifiesClientCertificates` — `internal/app/metrics_tls_test.go:22`, assertion `:69` | **E2-covered** |
| Missing client certificate rejected | `internal/app/tlsreload.go:299` | same test, `internal/app/metrics_tls_test.go:71-75` | **E2-covered** |
| Wrong CA (rogue issuer) rejected | `internal/app/tlsreload.go:300` | same test, `internal/app/metrics_tls_test.go:76-84` | **E2-covered** |
| Expired client certificate rejected | chain validation via `internal/app/tlsreload.go:299` | same test, `internal/app/metrics_tls_test.go:85-93` | **E2-covered** |
| **Revoked identity** | **NO CODE.** No CRL, no OCSP, no `VerifyPeerCertificate` hook anywhere. `grep -rn 'CRL\|OCSP\|RevocationList\|VerifyPeerCertificate' --include='*.go'` returns only unrelated CRLF comments at `internal/sse/sse.go:22` and `:124`. Revocation is achievable only by withdrawing the issuing CA from the client-CA bundle, which revokes *every* identity that CA issued. | none | **GAP — G-2** |
| Cert + client-CA rotation (overlap, then withdrawal) | `internal/app/tlsreload.go:275-308` — cert and CA pool published as one unit | `TestMetricsTLSHolderRotatesCertificateAndClientCATogether` — `internal/app/tlsreload_test.go:216`; overlap `:250-268`, withdrawal `:270-279` | **E2-covered** |
| Rollback on failed reload — **serving** listener | `internal/app/tlsreload.go:95-103` — bundle built whole, stored only on success | `TestCertificateHolderKeepsTheOldBundleWhenReloadFails` — `internal/app/tlsreload_test.go:145` | **E2-covered** |
| Rollback on failed reload — **metrics** listener | `internal/app/tlsreload.go:275-295` returns before `h.current.Store`, so the old config survives | **none.** All three `newMetricsTLSHolder` call sites (`internal/app/tlsreload_test.go:233`, `internal/app/tlsserve_test.go:75`, `internal/app/metrics_tls_test.go:37`) exercise only successful reloads | **E1-only — G-6** |
| Real PKI, real scraper, real rotation window | — | — | **REQUIRES-E4** (`admission-checklist.md:18`, "BLOCKED: target PKI") |

**Re-run at this SHA**: `go test ./internal/app/ -run 'TestMetricsTLSConfigRequiresAndVerifiesClientCertificates|TestMetricsTLSHolderRotatesCertificateAndClientCATogether' -count=1` → both PASS.

**Runbook/code divergence (documentation defect, G-7).**
`/Users/ziy/Code/ClayCosmos/Halro/docs/observability/operations-runbook.md:271-272` instructs:
*"Perform a controlled Halro restart; Metrics TLS material is intentionally loaded at listener
startup, so replacing files alone is not a reload."* That is **false at this SHA**: SIGHUP reloads
metrics TLS via `/Users/ziy/Code/ClayCosmos/Halro/internal/app/reload.go:153-154` and `:170`
(`apply(ReloadMetricsTLS, reloadMetricsTLS)`), and the capability is reported back to the operator
at `internal/app/reload.go:278`. An operator following the runbook takes two unnecessary restarts
per rotation. The runbook step is **stale E1**, not the procedure to execute in G3.

---

### (b) Credential dual-key overlap, hot reload, old key 401, restart/restore non-revival

Two halves with very different evidence.

#### (b.1) Bearer credential file (`/metrics`, audit anchor) — `internal/bearercred` — fully covered

| Sub-case | Enforcing code | Automated test | Level |
|---|---|---|---|
| Dual-key overlap | `internal/bearercred/credentials.go:250` `Rotate`; `:235` `authorize` | `TestRotateOverlapRevokeAndRestore` — `internal/bearercred/credentials_test.go:12`, overlap `:31-36` | **E2-covered** |
| Hot reload without restart | `internal/bearercred/credentials.go:120-147` `refresh`/`refreshLocked`; `:148` `signature` | same test (each `Authorize` re-reads) | **E2-covered** |
| Old key 401 after overlap expiry | `internal/bearercred/credentials.go:235-249` | same test, `internal/bearercred/credentials_test.go:38-40` | **E2-covered** |
| Explicit revoke → immediate refusal | `internal/bearercred/credentials.go:316` `Revoke`; `:519` `appendRevocation` | same test, `:41-47` | **E2-covered** |
| **Restore does not resurrect a revoked credential** | Append-only revocation ledger `internal/bearercred/credentials.go:359` (`<path>.revocations`) + hash-chained audit `:360`/`:448`/`:483`, consulted by `authorize` at `:235` | `TestRotateOverlapRevokeAndRestore` restores a pre-revocation file byte-for-byte and asserts it still fails — `internal/bearercred/credentials_test.go:48-53`; plus `TestRestoreCannotReuseVersionAnchoredInAudit` — `:137` | **E2-covered — strongest control in G3** |
| Credential audit tamper/truncate/reorder/delete | `internal/bearercred/credentials.go:404-486`, reorder check `:431-437` | `TestCredentialAuditDetectsTamperTruncationReorderingAndDeletion` — `internal/bearercred/credentials_test.go:80`, reorder `:119-127` | **E2-covered** |
| Concurrent rotation serialization | `internal/bearercred/lock_unix.go:13` | `TestConcurrentRotationsAreSerialized` — `:167` | **E2-covered** |
| File permission hardening | `internal/bearercred/credentials.go:181` | `TestRejectsBroadPermissionsAndCorruptState` — `:65` | **E2-covered** |

**Re-run at this SHA**: `go test ./internal/bearercred/ -run 'TestRotateOverlapRevokeAndRestore|TestRestoreCannotReuseVersionAnchoredInAudit|TestCredentialAuditDetectsTamperTruncationReorderingAndDeletion' -count=1 -v` → 3/3 PASS.

#### (b.2) Gateway Keys — the restore-non-revival property **does not hold**

| Sub-case | Enforcing code | Automated test | Level |
|---|---|---|---|
| Disabled / expired / invalid key refused | `internal/auth/snapshot.go:102-134` (`ErrInvalidKey`, `ErrKeyDisabled`, `ErrKeyExpired`, `ErrProjectDisabled`) | `TestSnapshotAuthentication` — `internal/auth/snapshot_test.go:100`; `TestInvalidKeyDoesNotCreateReservation` — `internal/gateway/service_test.go:1172` | **E2-covered** |
| Revocation not undone by a racing stale refresh | `internal/auth/snapshot.go:38-47` (`refreshMu` serializes read + install) | `TestStaleRefreshCannotResurrectARevokedKey` — `internal/auth/snapshot_test.go:59` | **E2-covered** |
| Revocation survives an admin client disconnect | — | `TestRevokedKeyStopsAuthenticatingWhenTheAdminClientDisconnects` — `internal/app/activation_context_test.go:47` | **E2-covered** |
| **Revocation survives a backup restore** | **NO MECHANISM.** A Gateway Key's revocation is only `Enabled bool` / `DeletedAt *time.Time` inside the bbolt metadata store (`internal/domain/models.go:462-474`). There is no revocation ledger, no revocation watermark and no anti-rollback check in restore — `grep -n 'monotonic\|anti-roll\|newer' internal/app/backup.go` returns only filesystem-rename rollback at `:267-290`, nothing about identity. Restoring a backup taken before a revocation re-enables the key. | **none.** `internal/app/backup_test.go:818` `TestRestoreInvalidatesCapturedAdminSessionsAndMFAChallenges` covers admin *sessions* only — nothing for Gateway Keys, provider credentials or KMS slots | **GAP — G-1, a plan §162/§218 red line** |

The compensating control exists only in prose:
`/Users/ziy/Code/ClayCosmos/Halro/docs/observability/operations-runbook.md:280-283` tells the
operator to "restore credential lifecycle state from a snapshot at or after the latest revocation
watermark; otherwise keep credentials revoked and rotate anew." That watermark exists for
`bearercred` and **does not exist for Gateway Keys** — the instruction is unexecutable for them.

---

### (c) Admin-plane RBAC allow/deny matrix + full audit

#### (c.1) Halro Admin plane — deny matrix exceptionally well covered, audit completeness is not

Role model is deliberately two-valued (`/Users/ziy/Code/ClayCosmos/Halro/internal/domain/admin.go:9-21`):
`administrator` and `read_only`, with an explicit design note that a per-endpoint permission matrix
was **rejected** in favour of one middleware rule the sweep can enforce. Enforcement:
`requireAdministratorRole` at `internal/app/admin_session.go:352-357`, reached through
`requireAdminMutation` (`:319-327`) and `requireAdminSetupMutation` (`:309-317`).

| Sub-case | Automated test | Level |
|---|---|---|
| **Deny matrix, exhaustive** — every registered non-GET `/admin/api/` route refuses a `read_only` session with 403 `read_only_role` | `TestReadOnlyRoleCannotReachAnyRegisteredMutationRoute` — `internal/app/admin_users_test.go:227`. Walks the live chi router (`:290`), bounds its exemption list against `readOnlyExemptionBudget` (`:273-277`), fails on an exemption matching no registered route (`:325-327`), and refuses to pass below 40 swept routes (`:321-323`). A real sweep, not a spot check. | **E2-covered** |
| Allow side — a `read_only` login works and can read | `TestCreateAdminUserRequiresStepUpAndProducesAWorkingReadOnlyLogin` — `internal/app/admin_users_test.go:53` | **E2-covered** |
| Unknown/empty role never treated as administrator | `TestEmptyRoleIsNeverValidAndNeverAdministrator` — `internal/app/admin_role_strictness_test.go:13`; `TestAdministratorGateIsAnExactComparison` — `:36` | **E2-covered** |
| Step-up re-auth on every destructive delete | `TestEveryDestructiveDeleteRequiresStepUp` — `internal/app/admin_stepup_test.go:53` (chi.Walk `:73`) | **E2-covered** |
| Step-up on every security-control edit | `TestEverySecurityControlEditRequiresStepUp` — `internal/app/admin_stepup_test.go:211` (sweep `:240`, floor of 7 `:280-282`) | **E2-covered** |
| Read-only cannot force an invocation-target refresh | `TestReadOnlySessionCannotForceAnInvocationTargetRefresh` — `internal/app/admin_invocation_targets_test.go:577` | **E2-covered** |
| CSRF on admin mutations | `TestAdminBootstrapLoginCSRFAndLogout` — `internal/app/admin_session_test.go:27`; `TestAdminDeveloperExecutionRequiresAdminCSRF` — `internal/app/admin_developer_test.go:130` | **E2-covered** |
| Failed password / MFA attempts throttled and audited | `TestPasswordChangeFailuresAreThrottledAndAudited` — `internal/app/admin_credential_guard_test.go:16`; `TestFailedMFACodesAreAuditedAndBounded` — `internal/app/admin_mfa_guard_test.go:31` | **E2-covered** |
| **"完整审计" — audit completeness across the mutation surface** | **NO SWEEP.** Only five `chi.Walk` sweeps exist in the tree (`internal/app/admin_users_test.go:290`, `internal/app/gateway_contract_test.go:104`, `internal/app/admin_contract_test.go:20`, `internal/app/admin_stepup_test.go:73` and `:240`) and none asserts an audit record. Audit coverage is per-feature only — `TestAdminDeveloperExecutionIsAudited` (`internal/app/admin_developer_test.go:174`), `TestDriftDetectionIsAudited` (`internal/app/capability_audit_test.go:149`), `TestReadingACapturedPayloadIsAudited` (`internal/app/admin_usage_failures_test.go:225`), `TestDueAccountingTimezoneChangeIsAppliedAndAudited` (`internal/app/admin_accounting_settings_test.go:311`), and ~20 more. A new mutation route writing no audit record passes every gate in the repo. | **GAP — G-4. Deny matrix E2-covered; audit completeness E1-only.** |

**Re-run at this SHA**: `go test ./internal/app/ -run TestReadOnlyRoleCannotReachAnyRegisteredMutationRoute -count=1` → PASS.

#### (c.2) Prometheus / Alertmanager management RBAC — no Halro code exists

`admission-checklist.md:20` asks for "Prometheus query/reload/admin and Alertmanager
UI/API/silence/config/reload deny anonymous and unauthorized identities". Those are external
components fronted by a target-environment identity proxy. Halro ships no code and no in-repo
test can exist. **REQUIRES-E4.**

---

### (d) KMS / Secret Store: normal, unavailable, recovered, rotated, wrong-key

Architecture: `internal/kms` owns a cloud-neutral contract with no SDK, credentials or
persistence; `internal/kms/awskms` is the one driver; `internal/masterkey` owns Slot transitions;
`internal/vault` does HKDF derivation and AES-256-GCM with audience-bound AAD.

| Scenario | Enforcing code | Automated test | Level |
|---|---|---|---|
| **Normal** — dual-Slot init, no plaintext key published | `internal/masterkey/slots.go`, `internal/app/kms_master_key.go` | `TestKMSInitializationPublishesIndependentVerifiedSlotsWithoutPlaintextKey` — `internal/app/kms_master_key_test.go:91` | **E2-covered** |
| Partial init never publishes a half-instance | — | `TestKMSInitializationFailureNeverPublishesPartialInstance` — `internal/app/kms_master_key_test.go:172` | **E2-covered** |
| Start does not auto-initialize or trust file presence | — | `TestKeySlotStartDoesNotAutoInitializeOrTrustFilePresence` — `internal/app/kms_master_key_test.go:224` | **E2-covered** |
| **Unavailable** — transient and permanent faults | fault injection `internal/kms/fakekms/fake.go:130-151`; typed taxonomy `internal/kms/contract.go` | `TestExecutorRetriesOnlyRetryableErrorsWithFullJitterBoundary` — `internal/kms/retry_test.go:35`; `TestExecutorFailsFastForPermanentErrors` — `:63`; `TestExecutorEnforcesCallTimeoutAndTotalDeadline` — `:79`; `TestAdminMasterKeyCustodyFailsClosedWhenKeySlotMetadataIsUnavailable` — `internal/app/admin_master_key_test.go:133` | **E2-covered** |
| **Recovered** — Recovery Slot repairs a dead Primary | `internal/app/kms_key_lifecycle.go` | `TestRecoveryRepairsPermanentlyUnavailablePrimaryBeforeColdStart` — `internal/app/kms_key_lifecycle_test.go:520`; `TestKMSRecoveryRewrapUsesPrimaryAsIndependentSource` — `:475`; `TestKMSRestoreRecoversWithExplicitRecoveryWhenPrimaryIsDisabled` — `internal/app/backup_test.go:170` | **E2-covered** |
| Break-glass requires exact confirmation + audit | — | `TestRecoverySlotRequiresExactConfirmationAndWritesBreakGlassAudit` — `internal/app/kms_master_key_test.go:331` | **E2-covered** |
| **Rotated** — rewrap and DEK rotation, idempotent at every kill point | — | `TestKMSRewrapPreservesMasterKeyCiphertextAndKeyVersion` — `internal/app/kms_key_lifecycle_test.go:28`; `TestKMSRewrapRecoversIdempotentlyAtEveryPublicationPoint` — `:142`; `TestKMSDEKRotationReencryptsAllMaterialAndPreservesAudit` — `:578`; `TestKMSDEKRotationRecoversIdempotentlyAtEveryPublicationPoint` — `:724`; `TestMasterKeyRotationRecoversFromEveryPublicationKillPoint` — `internal/app/key_rotation_test.go:177`; `TestMasterKeyRotationReencryptsCredentialsAndPreservesAuditChain` — `:24` | **E2-covered** |
| Rewrap fails closed on suspected compromise | — | `TestKMSRewrapFailsClosedForSuspectedCompromise` — `internal/app/kms_key_lifecycle_test.go:563` | **E2-covered** |
| **Wrong key** — structurally valid key from another vault | Vault Key Check in `masterkey.VerifySlot` | `TestKeySlotVerificationRejectsValidKeyFromAnotherVault` — `internal/app/key_slots_test.go:22` (asserts `masterkey.ErrVaultKeyMismatch`, `:58`); `TestKeySlotVerificationFailsClosed` — `internal/masterkey/slots_test.go:136`; `TestCredentialTamperFails` — `internal/vault/vault_test.go:44`; `TestProtectedPayloadRejectsEveryBindingAndFormatMutation` — `internal/kms/payload_test.go:32` | **E2-covered** |
| Restore with mismatched master key names both fingerprints | — | `TestRestoreMasterKeyMismatchNamesBothFingerprintsAndRecoveryStep` — `internal/app/backup_test.go:778` | **E2-covered** |
| Error taxonomy and telemetry carry no secret | — | `TestTypedErrorTaxonomyIsStableAndSecretSafe` — `internal/kms/contract_test.go:77`; `TestKMSMasterKeyCanaryNeverReachesPersistenceTelemetryErrorsOrHeapProfile` — `internal/app/kms_secret_canary_test.go:26` | **E2-covered** |
| **Real AWS KMS** — real IAM, region, key policy | — | `TestRealAWSDualSlotInitializeAndRecovery` — `internal/app/kms_real_smoke_test.go:26`; `TestRealAWSKMSKeyLifecycle` — `:108`; `TestRealAWSKMSDisasterRecovery` — `:236`. All gated behind `HALRO_AWS_KMS_DUAL_REAL=1` (`:27`) and skipped by default | **REQUIRES-E4**, harness exists |

**Verdict on (d):** the best-covered G3 item. Every plan-named scenario has a fault-injected test
against `fakekms`. What remains for E4 is only that the real KMS behaves as `fakekms` models it,
and the repo already ships the opt-in harness for exactly that.

---

### (e) SSRF, DNS/IP recheck, private/metadata addresses, redirects, egress allowlist

`internal/safetransport` is two files: `transport.go` (direct dialing) and `http_connect.go`
(admin-configured HTTP CONNECT egress proxy).

| Control | Enforcing code | Automated test | Level |
|---|---|---|---|
| HTTPS-only scheme | `internal/safetransport/transport.go:175` (scheme must be http/https), `:178` (`RequireHTTPS`); policy field `:26`; hard-wired `true` at every production call site — `internal/app/providers.go:36`, `internal/app/alerts.go:25`, `internal/modelcatalog/manager.go:143` | `TestValidateURLPolicy` subtest `"plaintext"` — `internal/safetransport/transport_test.go:79`, case `:84` | **E2-covered**, with a caveat below |
| Explicit host allowlist | construction-time normalize/dedupe `transport.go:55-65`; config-time `:191-193`; **dial-time second gate** `:236-238`; `hostAllowed` `:329-337`; `PolicyOf` copy `:157-168` | `TestValidateURLPolicy`/`"wrong host"` — `transport_test.go:88`; `TestDialerAllowlistRefusalSaysNothingWasSent` — `:244`; `TestPolicyOfReportsTheEffectiveAllowlistAsACopy` — `:287`; `TestBedrockRuntimeClientMayDialTheDerivedControlPlaneOnly` — `internal/app/providers_test.go:149` | **E2-covered** |
| DNS → IP validation and pinned dialing | `pinnedDialContext` `transport.go:229-261` — every resolved address validated `:254-258`, empty answer refused `:251-253`, dial goes to the **validated literal** `:260`; installed on both `DialContext` `:77` and `DialTLSContext` `:78` so TLS cannot escape it; proxy equivalent `http_connect.go:223-244` | `TestMixedPublicPrivateDNSAnswerIsRejectedBeforeDial` — `transport_test.go:34`; `TestDialUsesValidatedIP` — `:61` (asserts the dial address is literally `203.0.113.10:443`); `TestRefusalsInTheDialerSayNothingWasSent` — `:195`; `TestResolverFailureIsNotMarkedAsOurRefusal` — `:267`; proxy side `http_connect_test.go:23`, `:90`, `:171` | **E2-covered.** Note there is no separate "recheck" step: resolution happens inside the dialer on every dial and only a validated literal reaches the socket, which is stronger than a recheck — there is no window |
| Private / loopback / link-local / metadata / CGNAT / reserved / tunnel | `validateAddress` `transport.go:291-317`: unspecified/loopback/multicast/link-local `:296-298`; metadata named explicitly `:299-301` with constants `:286-289` (`169.254.169.254`, `100.100.100.200`); reserved+tunnel table `:269-278` applied `:302-306`; CGNAT `100.64/10` `:307-309`; RFC1918 + `fc00::/7` `:310-312`; catch-all `:313-315`; proxy variant `http_connect.go:258-267` | `TestReservedAndTunnelAddressesAreRefused` — `transport_test.go:143`: 12 refused addresses `:148-159`, CGNAT-under-`AllowPrivate` allow `:170`, **metadata still refused even under `AllowPrivate`** `:173`, public-still-allowed `:177`; `TestValidateURLPolicy` `"loopback"` `:86` and `"metadata"` `:87`; app level `TestPrivateProviderEndpointStaysRefusedByDefault` — `internal/app/private_endpoint_policy_test.go:51`, `TestRuntimeRejectsPrivateWebhookEndpointByDefault` — `internal/app/alerts_test.go:67` | **E2-covered except IPv4-mapped IPv6 — see G-8** |
| Redirect refusal | `transport.go:82-84` — `CheckRedirect` returns `http.ErrUseLastResponse` unconditionally | `TestClientIgnoresEnvironmentProxyAndRefusesRedirects` — `transport_test.go:112`, assertion `:134` | **E2-covered** |
| Environment proxy refusal | `transport.go:68` — `Proxy: nil` (Go's default is `ProxyFromEnvironment`) | same test, assertion `:126-129` (sets `HTTPS_PROXY` at `:113`, unwraps `pinnedTransport`, requires `Proxy == nil`) | **E2-covered** |
| Egress allowlist configuration surface | Global booleans `internal/config/config.go:617-618`; per-provider `AllowedHosts` `internal/domain/models.go:527` **derived from the validated endpoint host, not caller-supplied** (`internal/app/admin_providers.go:1794`, after `ValidateURL` `:1310`/`:1652`); per-webhook `internal/domain/models.go:1344` derived at `internal/app/admin_alerts.go:264` after `ValidateURL` `:257`; egress-proxy admin API `internal/app/admin_provider_egress.go:31`/`:106` with domain validation `internal/domain/models.go:253-290` and a cap of 32 `:229` | `TestProviderEgressProxyValidation` — `internal/domain/provider_egress_test.go:22` (8 rejection cases `:35-48`); `TestAdminManagedProviderEgressProxyIsEncryptedAndHotActivated` — `internal/app/provider_egress_test.go:59`; `TestRuntimeLoadsAudienceBoundEncryptedWebhook` — `internal/app/alerts_test.go:22`; `TestPrivateProviderEndpointStaysRefusedByDefault` — `internal/app/private_endpoint_policy_test.go:51` | **Mostly E2-covered — see G-9** |
| Core (Prometheus SD, remote write, Alertmanager webhook) egress | no Halro code | — | **REQUIRES-E4** (`admission-checklist.md:25`) |

**Untested-but-coded sub-cases inside safetransport** (each is code with no failing test):
1. **IPv4-mapped IPv6 unwrapping** — `transport.go:292` (`address.Unmap()`), also `:259-260`,
   `http_connect.go:143`, `:259`. `grep -rn '::ffff' --include='*.go' internal/safetransport/`
   returns **0 hits**; the only repo hits are the unrelated `internal/sourcelimit/aggregation_test.go:26,67`.
   Deleting the `Unmap()` at `transport.go:292` fails no test. (Verified independently.)
2. **Wire-level HTTPS** — HTTPS is enforced only in `ValidateURL`. `pinnedTransport` embeds
   `*http.Transport` without overriding `RoundTrip` (`transport.go:148-151`) and `pinnedDialContext`
   never inspects scheme. Every current caller does call `ValidateURL` first, but nothing pins that.
3. **"Allowed hosts must not be empty"** — `internal/domain/models.go:771` (provider) and `:1368`
   (webhook). These compensate for `transport.go:191`/`:236` treating an empty allowlist as
   "any host that passes the address checks". Neither rule has a test.
4. **Egress-proxy endpoint scheme rejection** — `http_connect.go:93`, `internal/domain/models.go:271`.
   `TestProviderEgressProxyValidation` covers kind, userinfo, port, path and cleartext-auth, but no
   case supplies e.g. `socks5://`.
5. **`internal/config` `Security` block has no validator at all** (`config.go:616-621`).

---

### (f) Audit tamper / delete / reorder / write-failure detection and alerting

**Mechanism.** `internal/audit` is a framed append-only log: header `HAUD`/v1 with big-endian
sequence, payload length, and the **SHA-256 of the previous frame**, plus a trailing
**HMAC-SHA256** over header+payload (`/Users/ziy/Code/ClayCosmos/Halro/internal/audit/log.go:20-36`,
`:307-319`). The chain is verified in exactly one function, `scan`
(`internal/audit/log.go:321-383`), reached by `Verify`/`VerifyWithVisitor` (`:158-183`), `Replay`
(`:251-286`) and `Open` (`:120-156`). Two anchors sit on top: a **bbolt checkpoint** of
(records, bytes, last hash) written after appends (`internal/app/runtime.go:1593-1600`, monotonicity
enforced at `internal/store/bolt/store_audit.go:393-418`), and **off-host anchors** (ADR 0015)
pulled by the dead-man probe (`internal/app/audit_anchor.go:28-76`, `internal/deadman/anchor.go:92-144`).

**Scope finding: the gateway data plane writes no audit records.** No file under `internal/gateway`
or `internal/gatewayapi` imports `internal/audit`. Everything below concerns the **admin plane**,
not inference traffic.

| Failure mode | Enforcing code | Automated test | Level |
|---|---|---|---|
| **(a) Content tampering** | per-frame HMAC compare `internal/audit/log.go:359-364`; magic/version `:338-340`; payload-length bound `:349-351`; event re-validation `:365-371` | `TestAuditDetectsTamperingAndWrongKey` — `internal/audit/log_test.go:180` (flips a byte, asserts `ErrCorrupt` `:207-209`; wrong-key `:193-195`) | **E2-covered** |
| Whole-chain rewrite with recomputed tail | off-host anchor comparison `internal/app/audit_anchor_verify.go:110-221` | `TestVerifyAuditAnchorsDetectsARewrittenChainWithARecomputedTail` — `internal/app/audit_anchor_verify_test.go:88` | **E2-covered** |
| **(b) Deletion / truncation of a committed suffix** | checkpoint comparison `internal/app/audit.go:73-80`; startup `reconcileAuditCheckpoint` `internal/app/runtime.go:1602-1616`; backwards movement refused in the store `internal/store/bolt/store_audit.go:410-415` | `TestAuditCheckpointDetectsDeletedSuffix` — `internal/app/runtime_test.go:304` (truncates the real audit file, asserts `VerifyAudit` fails `:317-321`); `TestAuditCheckpointPersistenceAndMonotonicity` — `internal/store/bolt/store_test.go:830` | **E2-covered** |
| Torn tail tolerated, gap not | `Open` truncates only an incomplete final frame `internal/audit/log.go:137-146`; `Verify` refuses a partial tail `:179-181` | `TestOpenTruncatesOnlyPartialAuditTail` — `internal/audit/log_test.go:212`; `TestLoadAuditAnchorsFileToleratesATornTailButNotAGap` — `internal/app/audit_anchor_gap_test.go:125` | **E2-covered** |
| **(c) Reordering / sequence gap — in the audit chain itself** | `sequence != summary.Records+1` → `ErrCorrupt` `internal/audit/log.go:341-344`; previous-hash linkage `:345-347`; `Replay` boundary equality `:282-284` | **NONE.** `internal/audit/log_test.go` contains exactly 8 test functions (`:14`, `:38`, `:84`, `:115`, `:151`, `:180`, `:212`, `:268`) and **not one constructs a reordered or gapped frame**. The tamper test trips the HMAC check first, so `log.go:341-347` is never the failing assertion. (Verified independently.) | **E1-only — G-3** |
| (c) Reordering — anchor series and credential chain (different chains) | `internal/app/audit_anchor_verify.go:183-187`, `:193-204`; `internal/deadman/anchor.go:110-125`; `internal/bearercred/credentials.go:431-437` | `TestVerifyAuditAnchorsReportsGapsAndRewinds` — `internal/app/audit_anchor_gap_test.go:24`; `TestAnchorSequenceGapIsReported` / `TestAnchorSequenceRewindIsReported` — `internal/deadman/anchor_health_test.go:112` / `:76`; `TestCredentialAuditDetectsTamperTruncationReorderingAndDeletion` — `internal/bearercred/credentials_test.go:80` | **E2-covered — but do not read these as covering `internal/audit`** |
| **(d) Write failure** — fail-closed on the admin plane | write error `internal/audit/log.go:234-236`; short-write loop `:385-397`; **fsync error returned before in-memory state advances** `:241-247`; handlers return 503 via `adminAuditError` `internal/app/admin_errors.go:81-83` (~20 call sites) and `internal/app/failure_capture.go:142-149` | `TestAPayloadReadIsRefusedWhenItCannotBeAudited` — `internal/app/failure_payload_audit_test.go:21` (asserts 503, `code == audit_unavailable`, and the prompt withheld, `:52-64`); `TestAdminPreferenceAuditFailureRollsBackServerState` — `internal/app/admin_ui_settings_test.go:172` (asserts 503 **and** that the mutation did not persist, `:194-207`) | **Partially E2-covered — see G-10** |
| (d) Real ENOSPC / EROFS / fsync error | — | **NONE.** Both fail-closed tests simulate the failure with `runtime.audit.Close()` (`failure_payload_audit_test.go:47`, `admin_ui_settings_test.go:186`), hitting the closed-log guard `internal/audit/log.go:219-221` rather than a write path. The type holds a concrete `*os.File` (`log.go:96`) with no injection seam. | **E1-only** |
| **Alerting on audit integrity failure** | **NONE.** The only four audit metrics are anchor-*emission* gauges/counters (`internal/app/metrics.go:290-297`); the only audit alert rule is `HalroAuditAnchorStale` (`deploy/observability/prometheus/alert-rules.yml:19-39`), which fires on stale or never-emitted anchors, **not** on a broken chain. A failed audit write raises no metric — only `logger.Error` + 503. A startup verification failure aborts startup (`internal/app/runtime.go:620-636`) rather than alerting, and `doctor` does not verify the chain (`internal/app/doctor.go:588-593`). (Verified independently.) | documented intent only at `docs/observability/admission-checklist.md:26` and `docs/observability/security-rfc.md:36-39` | **E1-only — G-2 tier, see G-5** |
| `halro audit verify` / `verify-anchor` CLI | dispatch `cmd/halro/main.go:855-906`; `app.VerifyAudit` `internal/app/audit.go:15-82` (lock, schema, master key, full HMAC+chain+sequence scan, checkpoint equality); `app.VerifyAuditAnchors` `internal/app/audit_anchor_verify.go:110-221` | Underlying functions well tested (`internal/app/audit_anchor_verify_test.go:15,65,88,138,159`; `internal/app/audit_anchor_gap_test.go:24,73,125`; `internal/app/runtime_test.go:304`). **No end-to-end CLI test** — `cmd/halro/main_test.go` has no audit test | **E2-covered at function level, E1-only at CLI level** |
| Rollback of *both* file and checkpoint by a master-key holder | acknowledged as out of scope — `docs/contracts/audit-integrity.md:24-27` ("tamper evidence, not non-repudiation"); in-process warning `internal/app/audit_anchor.go:135-147` | — | **REQUIRES-E4** (immutable audit platform, `admission-checklist.md:26`) |

**Deliberate, documented fail-open exceptions at the audit boundary** (correct, but they must be
named in the G3/G4 evidence so a reviewer does not read them as defects):
- Anchor emission is fail-open — `internal/app/audit_anchor.go:22-27`, `:52-54`, `:61-63`.
- Post-commit admin audit *delivery* is fail-open-with-retry, because the record is already durable
  in the same bbolt transaction as the mutation — `internal/app/admin_audit_intent.go:14-30`,
  `:77-96`, drained `:129-140`. Guarded by `TestActivationRecoveryDrainsAuditIntentsWhileNothingIsStale`
  — `internal/app/activation_recovery_test.go:40` (`:55-57` asserts a backlog does not make the
  runtime refuse traffic).
- The dead-man's anchor pull is fail-open so an unreachable sink cannot stall the heartbeat —
  `TestPullAnchorsUnreachableDoesNotBlockProbes` — `internal/deadman/anchor_test.go:144`.
- Some capability-detection appends discard the error outright — `internal/app/admin_model_capability_detections.go:891`,
  `:917`, `:934`, `:1059`; `internal/app/admin_outcomes.go:388`; `internal/app/runtime.go:797`.

---

### G3 summary

| G3 item | Halro half | Core (Prometheus/Alertmanager) half |
|---|---|---|
| (a) mTLS + rotation/rollback | **E2-covered except revocation (no CRL/OCSP) and metrics-reload rollback**; runbook is stale | REQUIRES-E4 (`admission-checklist.md:18`) |
| (b) Credential lifecycle | `bearercred` **E2-covered in full**; **Gateway Keys fail restore-non-revival** | REQUIRES-E4 (`admission-checklist.md:19`) |
| (c) RBAC + audit | deny matrix **E2-covered (exhaustive router sweep)**; audit completeness **E1-only** | REQUIRES-E4 (`admission-checklist.md:20`) |
| (d) KMS / Secret Store | **E2-covered across all five named scenarios** | REQUIRES-E4 for real KMS; harness exists |
| (e) SSRF / egress | **E2-covered for all 6 core controls**; 5 untested-but-coded sub-cases | REQUIRES-E4 (`admission-checklist.md:25`) |
| (f) Audit integrity | tamper + truncation **E2-covered**; **reorder E1-only; no integrity alert at all** | REQUIRES-E4 (`admission-checklist.md:26`) |

---

## 2. Secret-canary scanning mechanism

### 2.1 Frontend bundle scan

**`/Users/ziy/Code/ClayCosmos/Halro/web/scripts/check-artifacts.mjs`.**
- Scan root `web/scripts/check-artifacts.mjs:4` → `../../internal/webui/dist` (the **embedded**
  bundle, not `web/dist`). Walks **every file** recursively as UTF-8 (`:7-14`, `:33-39`) — JS
  chunks, CSS, `index.html`, SVG, fonts, everything; no extension filter.
- Any `.map` file is a hard failure (`:35`), and `sourceMappingURL=` is separately forbidden (`:26`).
- 13 forbidden literals, plain `String.includes`, **no regexes** (`:16-30`): `sk-HALRO_` (`:17`),
  `AIzaHALRO` (`:18`), `ASIA0123456789ABCDEF` (`:19`), `halro.canary.token` (`:20`),
  `provider-secret-canary` (`:21`), `gw_plaintext-canary` (`:22`), `csrf-canary` (`:23`),
  `password-canary` (`:24`), `correct horse battery staple` (`:25`), `sourceMappingURL=` (`:26`),
  `localStorage` (`:27`), `sessionStorage` (`:28`), `indexedDB` (`:29`).
- Design note worth recording: `:22-:24` are strings that exist **only** in
  `web/src/api.test.ts:18,33,46`, so their absence also proves test code was not bundled.
  `:27-:29` are not secret canaries — they enforce the CLAUDE.md "no browser-side secret storage"
  invariant.
- Wiring: `web/package.json:8` runs it as the last step of `npm run build`; CI at
  `.github/workflows/ci.yml:56` then drift-checks at `:58`; release at
  `.github/workflows/release.yml:217`/`:219`.
- `web/scripts/check-bundle.mjs` is a sibling **size** budget (500 KiB gzip, `:60-63`) and does
  **no** secret scanning.

### 2.2 Go side — the same scan, re-implemented

`TestEmbeddedBrowserArtifactsContainNoSecretOrPersistenceCanaries` —
`/Users/ziy/Code/ClayCosmos/Halro/internal/app/secret_canary_test.go:225`. Walks `../webui/dist`
(`:230`), fails on any `.map` (`:246-248`), checks the same 13 literals (`:231-236`), and guards
against vacuity with an emptiness check (`:259-261`). CI at `.github/workflows/ci.yml:68-69`.

The five primary canary suites:

| Test | Location | Surfaces |
|---|---|---|
| `TestSecretCanaryNeverReachesTelemetryPersistenceOrAdminSurfaces` | `internal/app/secret_canary_test.go:35` | gateway response body `:108-113`, logs `:173`, `/metrics` `:143-147`,`:174`, 12 admin routes + `/admin/` HTML `:150-165`, heap profile `:175-180`, **every file under the data dir after `Close()`** `:186-201` |
| `TestRecoveredPanicDoesNotExposePanicRequestOrAuthorizationCanaries` | `internal/app/secret_canary_test.go:204` | panic-recovery 500 body `:221`, panic log record `:222` |
| `TestKMSMasterKeyCanaryNeverReachesPersistenceTelemetryErrorsOrHeapProfile` | `internal/app/kms_secret_canary_test.go:26` | logs `:103`, **returned error string from a tampered unwrap** `:104`, `/metrics` `:105`, **raw audit log bytes** `:106-114`, heap profile `:115-120`, data dir `:126-138` |
| `TestContentCanaryNeverPersistsOutsideTheResponsePath` | `internal/app/content_canary_test.go:45` | deliberately **non**-credential-shaped canaries (`:25-43` explains why); logs `:236`, usage snapshot + `/metrics` + 8 admin read routes `:237`, data dir `:244-262`; anti-vacuity guard `:153-172` and `scanned == 0` guard `:263-265` |
| `TestEchoedAuthorizerSecretNeverLeavesTheProviderBoundary` | `internal/app/failure_capture_security_test.go:26` | gateway HTTP error body `:117`, the stored failure-capture record `:133-136`, the admin failure-payload endpoint read back by a read-only admin `:151-155`, logs `:156`, **replayed audit records** `:158-168`, data dir `:172-190` |

Engine level: `internal/safelog/shapes_test.go:40` `TestNoAttributeShapeCarriesACredentialToTheHandler`
(every slog attribute shape, and the base64 encoding of the canary, `:15-19`), `:99`, `:117`, `:130`;
`internal/safelog/fuzz_test.go:18` `FuzzRedactNeverLeaksSeededSecret`;
`internal/logging/error_file_test.go:133` `TestBothDestinationsAreRedacted` (`:147-156` — main log
**and** the separate error log).

### 2.3 Coverage matrix (plan §116)

| Surface | Verdict | Citation |
|---|---|---|
| **Application logs** | **COVERED**, multiply | `internal/app/secret_canary_test.go:173`; `internal/app/content_canary_test.go:236`; `internal/app/kms_secret_canary_test.go:103`; `internal/app/failure_capture_security_test.go:156`; `internal/logging/error_file_test.go:147-156` |
| **Prometheus metric labels** | **COVERED — the strongest surface.** Two complementary gates: value scan of the `/metrics` body (`internal/app/secret_canary_test.go:147`,`:174`; `internal/app/content_canary_test.go:210`,`:237`; `internal/app/kms_secret_canary_test.go:105`) **and** a structural allowlist — `assertMetricsExpositionContract` at `internal/app/metrics_contract_test.go:22` parses every sample line and fails on any label name outside a hardcoded set (`:27-40`), with `name` scoped to exactly one family (`:46-48`, enforced `:74-84`). That is precisely the high-cardinality / raw-value-in-label gate | as listed |
| **Audit records** | **COVERED** | raw bytes `internal/app/kms_secret_canary_test.go:106-114`; replayed `audit.Record` JSON `internal/app/failure_capture_security_test.go:158-168`; audit read API `internal/app/capability_audit_test.go:181-198`; alert audit `internal/app/alerts_test.go:89`,`:98`; plus the data-dir sweeps |
| **Error responses to the API caller** | **COVERED** | `internal/app/failure_capture_security_test.go:117` (real upstream 401 echoing the Authorization header, through the full router); `internal/gatewayapi/handler_test.go:456-458` (internal `Cause` suppressed); `internal/app/secret_canary_test.go:217-221` (panic → 500); `internal/gateway/capture_test.go:122`; `internal/app/kms_secret_canary_test.go:104` |
| **Backup archives (`.hmbk`)** | **COVERED NARROWLY — proves encryption, not redaction** | `TestEncryptedBackupCreateVerifyAndSecretConfidentiality` — `internal/backup/archive_test.go:17`, asserts the archive contains neither `provider-secret-backup-canary` nor even the string `manifest.json` (`:62-64`); `TestOfflineEncryptedBackupCapturesConsistentManifestAndAudit` — `internal/app/backup_test.go:240` asserts `configCanary` `:385`, `objectCanary` `:388`, master key `:395` absent. **No test decrypts a backup and scans the extracted plaintext**, and the data-dir sweeps cannot reach `.hmbk` because backups must be written outside the data dir (`internal/app/backup_test.go:479`) |
| **`internal/webui/dist`** | **COVERED, doubly** | `web/scripts/check-artifacts.mjs:16-39` (CI `.github/workflows/ci.yml:56`) and independently `internal/app/secret_canary_test.go:225-262` (CI `:68`) |
| **Traces** | **NO COVERAGE — because there is no tracing subsystem.** No OpenTelemetry dependency in `go.mod`, no `internal/trace`/`internal/otel`/`internal/telemetry` package, no span emission. Nothing to leak. `docs/architecture/threat-model.md:103` likewise omits traces | — |
| Go heap profile (bonus) | **COVERED** | `internal/app/secret_canary_test.go:175-180`; `internal/app/kms_secret_canary_test.go:115-120` |
| Alert/Contact-Point payload (plan §155) | **audit side covered, delivery side not** | `TestAlertAuditStoresOnlyStableMetadata` — `internal/app/alerts_test.go:89` (canary `sk-alert-payload-canary-0123456789` `:98`); real Contact Point is `BLOCKED` per `admission-checklist.md:21` → **REQUIRES-E4** |

### 2.4 Literal canary values (selected; full set in the cited files)

Credential-shaped, defined at `internal/app/secret_canary_test.go:27-32`:
`sk-HALRO_OUTPUT_CANARY_0123456789abcdef`, `AIzaHALRO0123456789abcdefghijk`,
`ASIA0123456789ABCDEF`, `Bearer halro.canary.token.0123456789`,
`sk-HALRO_PROVIDER_CANARY_0123456789abcdef`, `correct horse battery staple`; plus
`sk-HALRO_PANIC_CANARY_0123456789abcdef` (`:205`),
`M11KMS_MASTER_KEY_CANARY_1234567` (`internal/app/kms_secret_canary_test.go:24`, exactly 32 bytes,
checked `:34-36`), `sk-live-canary-must-never-reach-the-log`
(`internal/safelog/shapes_test.go:12`), `gw_<canary literal redacted; see internal/safelog/shapes_test.go>`
(`internal/logging/error_file_test.go:144`), `ABSKQmVkcm9ja0FQSUtleUNhbmFyeQ==`
(`internal/gateway/provider_failure_log_test.go:200`, deliberately outside safelog's pattern list
per `:168`), `opaque-authorizer-canary-value` (`internal/gateway/capture_test.go:111`),
`opaque-provider-credential-canary-value` (`internal/app/failure_capture_security_test.go:27`).

Content canaries, deliberately **not** credential-shaped, at `internal/app/content_canary_test.go:37-42`:
`HALRO-CONTENT-CANARY-PROMPT-quokka-lamplight` and five siblings; plus at-rest canaries
`canary-9f3a-do-not-store-in-the-clear` (`internal/gateway/inference_resources_service_test.go:1020`)
and `canary-4b71-prompt-must-not-be-readable` (`internal/gateway/deferred_response_test.go:206`).

Backup: `provider-secret-backup-canary` (`internal/backup/archive_test.go:23`),
`provider-object-backup-canary` (`internal/app/backup_test.go:254`),
`deferred-input-backup-canary` (`:259`), `backup-config-canary` (`:320`).

Frontend: `web/src/api.test.ts:18` `csrf-canary`, `:27` `csrf-rotated-canary`,
`:33` `gw_plaintext-canary`, `:46` `password-canary`, `:48` `idem-canary`.

---

## 3. Fail-closed at each boundary the plan calls out (§149, §214)

| Boundary | Enforcing code | Test that fails if it flips to fail-open | Verdict |
|---|---|---|---|
| **Auth** (Gateway Key) | `internal/auth/snapshot.go:102-134` — every unresolvable condition returns a typed refusal; no default-allow branch | `TestInvalidKeyDoesNotCreateReservation` — `internal/gateway/service_test.go:1172` (also asserts **no reservation and no provider call**, `:1180-1182`); `TestSnapshotAuthentication` — `internal/auth/snapshot_test.go:100`; `TestStaleRefreshCannotResurrectARevokedKey` — `internal/auth/snapshot_test.go:59` | **VERIFIED E2** |
| **Auth** (Admin plane) | `requireAdministratorRole` `internal/app/admin_session.go:352-357` — exact string comparison, no fallback | `TestReadOnlyRoleCannotReachAnyRegisteredMutationRoute` — `internal/app/admin_users_test.go:227`; `TestAdministratorGateIsAnExactComparison` — `internal/app/admin_role_strictness_test.go:36` | **VERIFIED E2** |
| **Budget / accounting** | A failed ledger apply marks the shared status unavailable (`internal/budget/manager.go:1324`, `internal/ledger/log.go:432`,`:859`,`:865`,`:878`, `internal/ledger/seal.go:93`…`:172`), and the gateway refuses on it; readiness also drops (`internal/app/runtime.go:2018-2024`) | `TestDurabilityFailurePreventsCurrentAndFutureProviderCalls` — `internal/gateway/service_test.go:638`: injects ENOSPC-write and EIO-fsync, asserts `accounting_unavailable`/503, **`f.adapter.calls == 0`** (no provider call) on both the current and the next attempt, and that the status latches (`:663-672`); `TestFailedApplyPutsAccountingIntoTheSharedUnavailableState` — `internal/budget/poisoned_apply_test.go:64`; `TestWriteAndSyncFailuresMakeAccountingUnavailable` — `internal/ledger/log_test.go:234`; `TestFailedAppendReleasesAdmittedSpend` — `internal/budget/admission_test.go:98` | **VERIFIED E2 — the best-evidenced boundary** |
| **Redaction** | A missing policy returns `ErrPolicyUnavailable` rather than a clean pass (`internal/redaction/engine.go:47-50`); an untraversable content kind refuses (`:365-372`); streaming tool-argument transforms refuse rather than corrupt (`:687-690`) | `TestAMissingPolicyRefusesInsteadOfPassingTheTextThrough` — `internal/redaction/engine_test.go:91`; `TestOutboundRedactionRefusesAContentKindItCannotTraverse` — `internal/redaction/provider_tool_content_test.go:129`; `TestOutboundPrivateKeyFailsClosedAndCompatibilityWrapperReturnsNoMaterial` — `internal/redaction/engine_test.go:64`; `TestRollingStreamRejectsTransformInParallelToolArgumentFragments` — `internal/redaction/stream_test.go:143` | **VERIFIED E2** |
| **Transport** | Refusals are marked `ErrRefusedBeforeSend` so an ambiguous outcome is never inferred (`internal/safetransport/transport.go:237`); allowlist checked at both config and dial time (`:191`, `:236`); every DNS answer validated (`:254-258`) | `TestDialerAllowlistRefusalSaysNothingWasSent` — `internal/safetransport/transport_test.go:244`; `TestRefusalsInTheDialerSayNothingWasSent` — `:195`; `TestResolverFailureIsNotMarkedAsOurRefusal` — `:267`; `TestHTTPConnectDialerCloseClearsAndFailsClosed` — `internal/safetransport/http_connect_test.go:152` | **VERIFIED E2**, with the 5 untested sub-cases in §1(e) |
| **Audit** — audit-before-effect | Synchronous batch append blocks on the result (`internal/app/admin_session.go:493-517`, `:531-541`); handlers return 503 | `TestAPayloadReadIsRefusedWhenItCannotBeAudited` — `internal/app/failure_payload_audit_test.go:21` (asserts 503, `audit_unavailable`, and the prompt withheld); `TestAdminPreferenceAuditFailureRollsBackServerState` — `internal/app/admin_ui_settings_test.go:172` (asserts 503 **and** non-persistence) | **PARTIALLY VERIFIED — 2 of ~22 paths** |
| **Audit** — the other ~20 `adminAuditError` handlers and the login path | `internal/app/admin_errors.go:81-83` (call sites in `admin_settings.go:50`, `admin_deployments.go:97`,`:185`,`:362`, `admin_prices.go:227`,`:276`,`:429`,`:549`,`:624`,`:650`, `admin_projects.go:244`, `admin_redaction.go:235`, `admin_token_guard.go:253`, `admin_alerts.go:230`, `admin_resources.go:371`, and more); login `internal/app/admin_session.go:127-131` | **NO TEST.** Stated plainly: nothing in the repo would fail if any of these dropped the audit error. `TestRejectedLoginSprayCannotSetTheAuditAppendRate` (`internal/app/admin_session_test.go:508`) only asserts the rate limiter runs ahead of the audit path; its comment at `:505-506` records the fail-closed assumption without testing it | **E1-only — G-10** |
| **Audit** — startup refusal on a corrupt chain | `internal/app/runtime.go:620-636` | **NO TEST.** No test starts a runtime against a tampered audit file; `TestAuditCheckpointDetectsDeletedSuffix` truncates and then calls `VerifyAudit`, never `Open` (`internal/app/runtime_test.go:316-321`) | **E1-only** |

**Re-runs at this SHA (all PASS):**
`go test ./internal/gateway/ -run 'TestDurabilityFailurePreventsCurrentAndFutureProviderCalls|TestInvalidKeyDoesNotCreateReservation' -count=1` and
`go test ./internal/redaction/ -run 'TestAMissingPolicyRefusesInsteadOfPassingTheTextThrough|TestOutboundRedactionRefusesAContentKindItCannotTraverse' -count=1`.

---

## 4. Is `internal/safetransport` the only egress path?

**For every production path that can reach a provider, an alert webhook, or the model catalog: yes,
and it is enforced structurally rather than by convention.** Every adapter takes an injected
`*http.Client` and **refuses `nil`** — there is no `http.DefaultClient` fallback anywhere in
`internal/provider/*`: `internal/provider/openai/adapter.go:125`,
`internal/provider/anthropic/adapter.go:95`, `internal/provider/bedrock/adapter.go:102`,
`internal/provider/bedrockmantle/adapter.go:71`, `internal/provider/gemini/adapter.go:93`,
`internal/alert/dispatcher.go:39`, `internal/modelcatalog/manager.go:141`.
The only production producers of those clients are `internal/app/providers.go:837`,
`internal/app/alerts.go:89` and `internal/modelcatalog/manager.go:149` — all `safetransport.NewClient`.

### 4.1 `needs review` — one hit

**`/Users/ziy/Code/ClayCosmos/Halro/internal/kms/awskms/adapter.go:31-47`, `func New`.**
`awsconfig.LoadDefaultConfig` (`:31`) plus `servicekms.NewFromConfig` (`:42`) build an AWS SDK v2
HTTP client for which Halro supplies **no transport**. It therefore honours
`HTTP_PROXY`/`HTTPS_PROXY`/`AWS_CA_BUNDLE`, follows the SDK's own redirect policy, resolves and
dials without the address allowlist, and reaches IMDS (`169.254.169.254`, ECS `169.254.170.2`)
through the default credential chain — the one address family `safetransport` refuses even under
`AllowPrivate` (`internal/safetransport/transport.go:299-301`, asserted at
`internal/safetransport/transport_test.go:173`). The only destination knob is
`options.Endpoint` → `value.BaseEndpoint` (`awskms/adapter.go:43-45`). (Verified independently.)

**Verdict: `needs review`, not a defect.** This is a deliberate architectural carve-out documented
at `docs/adr/0010-kms-sdk-dependency-isolation.md`, and the reachable destinations are AWS KMS plus
IMDS rather than attacker-chosen URLs. But it is a genuine egress path outside safetransport with
no SSRF-shaped test, and G3 §146 asks specifically about metadata addresses — so it belongs in the
G3 evidence as a named, accepted exception rather than being silently covered by "safetransport is
the only egress path". `internal/kms/awskms/errors.go:67-81` uses `net/http` for status-code
classification only, not egress. `internal/vault` contains **zero** network constructs.

### 4.2 `legitimate` — enumerated by category

- **Loopback-only, production**: `cmd/halro/main.go:1215` `runHealthcheck` and `:1242`
  `runHealthcheckWithClient` (loopback forced `:1233-1239`, scheme/userinfo/query/fragment rejected
  `:1228-1231`); `cmd/halro/stats.go:204`/`:212` `metricsSampleFetcher` (loopback `:197-203`,
  `Proxy: nil` + redirects disabled `:205-210`).
- **In-process, no socket**: `internal/app/admin_developer.go:52` — builds a request with a
  **relative path** (`:44-49`) handed straight to `r.gatewayHandler().ServeHTTP` (`:82`).
- **Request construction against an injected safetransport client** (24 sites — the construction
  half, not an independent path): `internal/alert/dispatcher.go:404`;
  `internal/modelcatalog/manager.go:335`; `internal/provider/anthropic/adapter.go:173`,`:696`;
  `internal/provider/anthropic/batches.go:223`; `internal/provider/bedrock/adapter.go:365`,`:444`,`:673`;
  `internal/provider/bedrock/inference_resources.go:233`; `internal/provider/bedrock/models.go:92`;
  `internal/provider/bedrockmantle/adapter.go:93`,`:139`,`:304`;
  `internal/provider/gemini/adapter.go:146`,`:207`,`:274`,`:324`,`:534`;
  `internal/provider/openai/adapter.go:310`,`:393`,`:544`,`:597`,`:811`;
  `internal/provider/openai/inference_resources.go:292`.
- **Deadman — deliberately outside Halro's failure domain, separate binary and image**:
  `internal/deadman/http.go:39-46` `secureClient` (own transport, `Proxy: nil` `:39`, pinned CA pool
  `:24-28`, optional mTLS `:29-38`, TLS 1.2 floor, redirects forbidden `:43-45`), `:82` `request`,
  `:99`, `:124`; `internal/deadman/anchor.go:146`; binary `cmd/halro-deadman/main.go:13`. Its
  dependency isolation is itself enforced by `deploy/observability/deadman_image_test.go:30` and `:74`.
- **Test / load / build tooling outside the shipped binaries**: `tests/soak/main.go:119`,`:212`,`:239`;
  `tests/stress/stream_test.go:76`,`:81`,`:100`; `tests/compatibility/server/main.go:247-273`
  (inbound only). `tools/**` has **zero** `http.`/`net.` hits.
- **Test-only fake `RoundTripper`** (never opens a socket) — ~100 sites across `internal/app`,
  `internal/alert`, `internal/modelcatalog`, `internal/provider/*`, `internal/gateway`, `cmd/halro`.
  The only two `http.DefaultClient` uses in the entire repo are
  `internal/provider/openai/adapter_test.go:514` and `:528`, which build an adapter purely to inspect
  its authorizer in `TestProviderAuthorizationCannotBeOverriddenByExistingHeaders`; no request is sent.
- **Test-only loopback TLS**: `internal/app/metrics_tls_test.go:115` `runTLSHandshake`;
  `internal/app/tlsserve_test.go:40`, `:91`.
- **Opt-in real-provider smoke tests** — these do bypass safetransport and reach real upstreams, but
  each is env-gated and skipped by default, so they never run in CI or `go test ./...`:
  `internal/provider/openai/real_smoke_test.go:62` (`HALRO_REAL_PROVIDER_SMOKE=1`, `:34-35`),
  `internal/provider/anthropic/real_smoke_test.go:59`, `internal/provider/bedrock/real_smoke_test.go:44`,
  `internal/provider/gemini/real_smoke_test.go:43`, `internal/provider/bedrockmantle/real_smoke_test.go:65`,`:187`,
  `internal/provider/openai/minimax_real_smoke_test.go:61`,`:188`,`:246`,`:317`,
  `internal/provider/anthropic/minimax_real_smoke_test.go:56`,
  `internal/provider/openai/bigmodel_real_smoke_test.go:52`,
  `internal/provider/openai/media_smoke_test.go:59`, `internal/kms/awskms/real_smoke_test.go:19`.

### 4.3 Patterns with zero hits repo-wide

`http.Get(`, `http.Post(`, `http.Head(`, `net.DialTimeout`, `httputil.NewSingleHostReverseProxy`,
`grpc.Dial`, `websocket`, `smtp.`, `http.DefaultTransport` — none anywhere. `exec.Command` hits are
all Go-tooling / `lsof` / `ps`, never a network client.

---

## 5. Is `internal/failurecapture` still the single deliberate secret-retention exception?

### 5.1 What failurecapture stores and how it is bounded

Package doc `/Users/ziy/Code/ClayCosmos/Halro/internal/failurecapture/failurecapture.go:1-28`:
the request a failed call carried plus a structured description of the upstream answer.

| Property | Mechanism | Test |
|---|---|---|
| Encrypted | `Sealer` interface `failurecapture.go:149-154`; implemented by `vault.EncryptFailurePayload` `internal/vault/vault.go:108-110` → `encryptScoped("failure-payload", …)` `:144` (AES-256-GCM, HKDF-derived subsystem key) | `TestACapturedFailureRoundTrips` — `internal/failurecapture/failurecapture_test.go:61` |
| **Bound to request + project** (AAD) | `encryptScoped(kind, id, audience, …)` `internal/vault/vault.go:108-114`, rationale `:103-107` — a record renamed onto another request ID, or lifted into another install, fails to open | `TestACaptureCannotBeOpenedUnderAnotherProject` — `failurecapture_test.go:82`; `TestACaptureNamesTheProjectThatSealedIt` — `:500`; `TestACaptureRefusesARequestIDThatIsNotAFileName` — `:98`; `TestACaptureRefusesIdentifiersCarryingTheNameSeparator` — `:520` |
| Bounded per record | `Options.MaxBytes` `failurecapture.go:157-160`, default 64 KiB `internal/config/config.go:486`, range-checked 1 KiB–1 MiB `internal/config/config.go:1401` | `TestAnOversizedCaptureIsTruncatedAndFlagged` — `:120`; `TestPrepareRecordAppliesTheByteCeilingBeforeAsyncOwnership` — `:155`; `TestTheCeilingBoundsWhatIsStoredNotWhatWasCut` — `:430` |
| Bounded per day | `Options.MaxRecordsPerDay` `failurecapture.go:161-164`; count seeded from the day's directory on `Open` so a restart cannot replenish it (`:183-187`) | `TestCaptureStopsAtTheDailyCeilingAndSaysSoOnce` — `:184`; `TestDailyCeilingAndMetricsSurviveRestart` — `:215` |
| Expiring | `Options.Retain` `failurecapture.go:165-168` — **zero is refused**; day directories purged whole; purge loop `internal/app/failure_capture.go:67`, `internal/app/runtime.go:1171`,`:1186` | `TestRetentionExpiresEachRecordOnTheConfiguredWindow` — `:270`; `TestExpiredCaptureIsUnreadableBeforePhysicalPurge` — `:332` |
| Best-effort, never fails the request | `failurecapture.go:24-27`; publication is whole-or-nothing `:397-410` | `TestFailedWriteDoesNotIncreaseCapturedMetric` — `:243`; `TestACaptureIsPublishedWholeAndLeavesNoTemporaryFile` — `:464` |
| Failures only, never a successful call | `failurecapture.go:18-20` | — |
| Permissions `0700`/`0600` | `failurecapture.go:50-53` | `TestCapturesAreAsPrivateAsTheDataDirectory` — `:357`; `TestOpenRefusesAStoreWithoutItsBounds` — `:404` |
| Reading is an audited admin action, and **fails closed** if it cannot be audited | `internal/app/failure_capture.go:136-150` | `TestReadingACapturedPayloadIsAudited` — `internal/app/admin_usage_failures_test.go:225`; `TestAPayloadReadIsRefusedWhenItCannotBeAudited` — `internal/app/failure_payload_audit_test.go:21` |
| Nothing leaks from it | — | `TestEchoedAuthorizerSecretNeverLeavesTheProviderBoundary` — `internal/app/failure_capture_security_test.go:26` |
| Disabled by default | `internal/app/failure_capture.go:26` | — |

### 5.2 **It is no longer the only such store.** `internal/gateway/inference_resources_store.go` also persists caller-written bodies.

This is a real finding, and the code itself says so. `internal/vault/vault.go:116-133` introduces
`EncryptResourceObject` with the comment (`:120-126`):

> *"These bytes are the same class of material `EncryptFailurePayload` guards: a prompt the caller
> wrote, or output a model produced. They used to be written to the object directory in the clear,
> which held them to a lower standard than the identical bytes captured from a failed request."*

The store is `internal/gateway/inference_resources_store.go` — `writeResourceObject` at `:145-175`,
sealer interface `:127-130`, roles `content`/`input` at `:136-138`, scope binding
`objectScope(resourceID, role)` at `:140`, read path `:211`, erase `:214`. It holds **uploaded
batch input files, batch results, and deferred/async request bodies and responses**.

| Property | Resource objects | failurecapture |
|---|---|---|
| Encrypted under the master key | **yes** — `inference_resources_store.go:151` | yes |
| Bound to resource + project (AAD) | **yes** — `objectScope` `:140`, seal `:151`, plus role in the scope so a request cannot be renamed over its answer (`:133-135`) | yes (request + project) |
| Per-object size ceiling | **indirect only** — bounded by `server.max_request_bytes` (default 10 MiB, `internal/config/default.go:34`) or the Project's `MaxRequestBytes` (`internal/app/runtime.go:610`). No dedicated object ceiling | explicit `MaxBytes`, 64 KiB default, range-checked |
| Per-day count ceiling | **NONE** | explicit `MaxRecordsPerDay` |
| Expiry | **yes** — `ExpiresAt` set at creation: 30 days for files (`:339`), 7 days for batches (`:824`) and async invokes (`:1217`); reaped by `CleanupExpiredProviderResource` `:683-684` | `Retain`, zero refused |
| Successful calls captured | **yes — this is normal-path traffic, not a failure tail** | no, failures only |
| Reading is an audited admin action | **no** — read through the owning Project's own API | yes |
| Enabled by default | **yes, whenever the resource endpoints are used** | no |
| At-rest encryption tested | `internal/gateway/inference_resources_service_test.go:1020-1035` (canary `canary-9f3a-do-not-store-in-the-clear`); `internal/gateway/deferred_response_test.go:206-216` (canary `canary-4b71-prompt-must-not-be-readable`); rotation `internal/app/retained_ciphertext_rotation_test.go:67`,`:98`; backup `internal/app/backup_test.go:254`,`:259` | yes |

**Assessment.** The *security posture* of the resource-object store is sound — sealed, bound,
expiring, and canary-tested. What is wrong is the **stated invariant**, which is a live claim in
`CLAUDE.md` ("`internal/failurecapture` — the one place Halro stores what a caller wrote"; "The
single deliberate exception is `internal/failurecapture`") and in the package doc at
`internal/failurecapture/failurecapture.go:5-6`. At this SHA that claim is **false**, and the
difference is not cosmetic: resource objects retain successful, normal-path prompts and completions
by default, for 7-30 days, with no daily count ceiling and no audited-read requirement — a
materially larger retention surface than the bounded failure tail the invariant describes. A
Security reviewer signing G1/G3 against the written invariant would be signing against a
description of the system that no longer matches it.

No **third** store exists: outside these two, the only other at-rest caller content is the
`.hmbk` backup (encrypted, `internal/backup/archive_test.go:17`), and the data-dir canary sweeps
(`internal/app/content_canary_test.go:244-262`, `internal/app/secret_canary_test.go:186-201`) assert
that nothing else under the data directory holds it in the clear.

---

## 6. Gaps, most severe first

**G-1 — A backup restore resurrects revoked Gateway Keys. Plan §162/§218 red line.**
Gateway Key revocation is only `Enabled bool` / `DeletedAt` in the bbolt metadata store
(`internal/domain/models.go:462-474`). There is no revocation ledger, no revocation watermark and
no anti-rollback check in `RestoreBackup` (`internal/app/backup.go:147-303`; `grep -n
'monotonic\|anti-roll\|newer'` finds only filesystem-rename rollback at `:267-290`). Restoring a
backup taken before a revocation re-enables that key, and nothing detects it. `internal/bearercred`
solves exactly this problem correctly (`credentials.go:359`,`:519`, proven at
`internal/bearercred/credentials_test.go:48-53`) — the pattern exists in-repo and was not applied
to Gateway Keys, provider credentials, or KMS slots. The runbook's compensating instruction
(`docs/observability/operations-runbook.md:280-283`, "restore from a snapshot at or after the
latest revocation watermark") is **unexecutable** for Gateway Keys because no such watermark exists.
The plan lists this as an unwaivable red line. **This should block G3 and G4 sign-off.**

**G-2 — mTLS client-certificate revocation does not exist.**
No CRL, no OCSP, no `VerifyPeerCertificate` hook anywhere (`internal/app/tlsreload.go:296-301`).
G3 §142 asks explicitly for "被吊销的 mTLS 身份". The only available action is withdrawing the
issuing CA, which revokes every identity that CA issued — an all-or-nothing control that the
rotation test itself demonstrates (`internal/app/tlsreload_test.go:270-279`). Either scope the item
to "revocation by CA withdrawal", with the operational consequence written down, or accept it as an
open gap. Do not record §142 as PASS on the strength of the expired-certificate case.

**G-3 — `internal/audit` reordering and sequence-gap detection is E1-only.**
The code exists (`internal/audit/log.go:341-347`) and `docs/contracts/audit-integrity.md:12-14`
claims the property, but `internal/audit/log_test.go` has exactly 8 test functions and none
constructs a reordered or gapped frame; the tamper test trips the HMAC check first, so those lines
are never the failing assertion (verified independently). G3 §147 asks for "顺序异常" detection.
The equivalent claim **is** tested for the credential chain
(`internal/bearercred/credentials_test.go:119-127`) — do not let that row stand in for `internal/audit`.

**G-4 — No integrity alert for the audit trail, and no sweep proving admin mutations are audited.**
Two halves of the same weakness. (i) The only audit metrics are anchor-emission gauges
(`internal/app/metrics.go:290-297`) and the only audit alert rule is `HalroAuditAnchorStale`
(`deploy/observability/prometheus/alert-rules.yml:19-39`), which fires on stale anchor *emission*,
not on a broken chain; a failed audit write raises no metric at all, and a startup verification
failure aborts startup rather than alerting (`internal/app/runtime.go:620-636`). G3 §147 asks for
detection **and alerting**; only detection exists. (ii) No test sweeps the admin router asserting
every mutation writes an audit record — the five `chi.Walk` sweeps in the tree cover RBAC, step-up
and route registration only. A new mutation route that audits nothing passes every gate.
G3 §144's "完整审计" is therefore **E1-only**.

**G-5 — The AWS KMS SDK client is a genuine egress path outside safetransport.**
`internal/kms/awskms/adapter.go:31-47` builds an SDK client with no supplied transport, so it
honours environment proxies, follows SDK redirects, dials without the address allowlist, and
reaches IMDS — the one address family safetransport refuses even under `AllowPrivate`
(`internal/safetransport/transport.go:299-301`). Deliberate per
`docs/adr/0010-kms-sdk-dependency-isolation.md` and low-risk in practice, but G3 §146 asks about
metadata addresses specifically, so it must be recorded as a **named accepted exception** in the G3
evidence, not covered by a blanket "safetransport is the only egress path".

**G-6 — Audit fail-closed is tested on 2 of ~22 paths; metrics-TLS reload rollback on none.**
The audit-before-effect refusal is proven for the failure-payload read
(`internal/app/failure_payload_audit_test.go:21`) and the preference mutation
(`internal/app/admin_ui_settings_test.go:172`), and both simulate the failure with `audit.Close()`
rather than a real ENOSPC/EROFS/fsync error. The ~20 other `adminAuditError` handlers
(`internal/app/admin_errors.go:81-83`) and the login path
(`internal/app/admin_session.go:127-131`) have no test — nothing fails if any drops the error.
Separately, `metricsTLSHolder.reload` keeps the old config on failure by construction
(`internal/app/tlsreload.go:275-295`) but no test exercises a failed metrics reload, unlike the
serving listener's `TestCertificateHolderKeepsTheOldBundleWhenReloadFails`.

**G-7 — `CLAUDE.md` and the failurecapture package doc state an invariant that is false at this SHA.**
`internal/gateway/inference_resources_store.go:145-175` persists caller-written prompts and model
output for batch inputs, batch results and deferred/async responses — sealed and bound
(`internal/vault/vault.go:127-133`), but for **successful, normal-path** traffic, by default, for
7-30 days (`inference_resources_store.go:339`,`:824`,`:1217`), with **no daily count ceiling** and
no audited-read requirement. The vault comment at `internal/vault/vault.go:120-126` calls it "the
same class of material". Fix the invariant text (and decide whether the resource store should carry
failurecapture's count ceiling and audited read), rather than letting a reviewer sign G1 against a
description that no longer matches the system.

**G-8 — Runbook contradicts the code on metrics TLS rotation.**
`docs/observability/operations-runbook.md:271-272` says metrics TLS is startup-only and requires a
restart. SIGHUP reloads it (`internal/app/reload.go:153-154`, `:170`, advertised at `:278`). An
operator following the runbook takes two unnecessary restarts per rotation. Fix the runbook before
it is used as the G3 §142 procedure.

**G-9 — Five coded-but-untested sub-cases in the SSRF boundary.**
(i) IPv4-mapped IPv6 unwrapping — `internal/safetransport/transport.go:292`, `:259-260`,
`http_connect.go:143`,`:259`; `grep -rn '::ffff' internal/safetransport/` returns 0 hits, so
deleting the `Unmap()` fails nothing (verified independently). This matters because
`::ffff:169.254.169.254` is the classic metadata-SSRF bypass. (ii) HTTPS is enforced only in
`ValidateURL`, never on the wire (`transport.go:148-151` adds no scheme check). (iii) The domain
"allowed hosts must not be empty" rules (`internal/domain/models.go:771`, `:1368`) are untested,
and an empty allowlist means "any host that passes the address checks"
(`transport.go:191`, `:236`). (iv) The egress-proxy endpoint **scheme** rejection
(`http_connect.go:93`, `internal/domain/models.go:271`) is untested. (v) `internal/config`'s
`Security` block (`config.go:616-621`) has **no validator at all**.

**G-10 — Backup canary coverage proves encryption, not redaction.**
`internal/backup/archive_test.go:17` and `internal/app/backup_test.go:240` assert the `.hmbk` bytes
are opaque, but no test decrypts a backup and scans the extracted plaintext against the secret and
content canary sets — and the data-dir sweeps structurally cannot reach it, because backups must be
written outside the data dir (`internal/app/backup_test.go:479`). Plan §116 asks that canaries not
appear "in evidence packages"; a restored backup is exactly such a package.

**G-11 — `make check` does not run the frontend bundle scanner.**
`Makefile:181` (`check: fmt-check test race vet frontend-test observability-check`) omits it;
only `make full-check` (`Makefile:183` → `scripts/check-web-bundle.sh:27`) does. The default local
gate keeps `internal/webui/dist` scanned only because the Go duplicate at
`internal/app/secret_canary_test.go:225` exists. The two forbidden-literal lists
(`web/scripts/check-artifacts.mjs:16-30` and `internal/app/secret_canary_test.go:231-236`) are
maintained by hand with nothing enforcing that they stay in sync — deleting the Go test as
"redundant" would silently remove dist coverage from `make check`. Minor, but it is a
single-point-of-failure in the canary program.

---

## 7. Items correctly classified REQUIRES-E4 (no in-repo evidence is possible)

Not gaps — recorded so they are not mistaken for repository work left undone:
- Real PKI for metrics mTLS — `docs/observability/admission-checklist.md:18`.
- Real Secret Store for credential lifecycle — `admission-checklist.md:19`.
- Prometheus/Alertmanager management RBAC via the target identity proxy — `admission-checklist.md:20`.
- Real Contact Point `firing`/`resolved` with a canary-free payload — `admission-checklist.md:21` (plan §155).
- Independent dead-man and its failure-domain separation — `admission-checklist.md:22-23`.
- Core service-discovery / remote-write / webhook egress policy — `admission-checklist.md:25`.
- External immutable audit platform, and rollback of file + checkpoint together by a master-key
  holder (explicitly out of scope per `docs/contracts/audit-integrity.md:24-27`) — `admission-checklist.md:26`.
- Real AWS KMS behaviour — harness exists and is env-gated (`internal/app/kms_real_smoke_test.go:26`,`:108`,`:236`).
- Real provider endpoints for G2 — env-gated harnesses listed in §4.2.
