# Security review — 2026-09-05

Baseline: `381743f6613607dc256828f4776b52af8bdd232c` (HEAD verified). Reviewer: independent security role. Scope: auth/adminauth, hostsecurity/safetransport, vault/masterkey/kms/bearercred, audit, contentscan/redaction/tokenguard/safelog, and their reachable Admin/security integration paths. Instructions: repository AGENTS.md and approved review-plan.md. No other current reviewers' reports read; no memories used.

**Recommendation: do not release without disposition and independent verification of SEC-01 and SEC-02.** Both have deterministic local reproductions. SEC-03/04 are reproduced P2 defects. SEC-05 is a source-confirmed resource-bound defect without a load experiment. C2 remains a product contract dispute, not an authorization bypass finding. These are this reviewer's proposed severities; no second reviewer has yet adjudicated them.

Critical limitations: no real KMS/cloud/provider calls; no production or browser acceptance; no full gate duplicated. Object rotation reproduced through the public file-mode rotation API, actual failure-capture store, and production Vault object cryptography, not a full Files/Responses HTTP journey. KMS DEK object impact is source-traced, not separately reproduced. Parent owns integration gates, binary/browser/SDK execution and soak.

## Baseline and evidence handling

- Toolchain: `go version go1.26.6 darwin/arm64`. Evidence collected 2026-09-05, completed around `2026-09-05T10:07:40Z` UTC before report writing.
- `git rev-parse HEAD` returned the required SHA, exit 0. Later `git status --short`, exit 0, showed `M docs/review/README.md` and `?? docs/review/260905/`; these shared documentation changes were not altered by this reviewer except this report. No production code, tracked tests, Git index/ref, or user data was written.
- Reproductions and logs: `/private/tmp/halro-security-review-260905/`. Go overlays introduce test-only files; production source is unchanged. Test data uses existing `t.TempDir()` fixtures. Fixed fixture secrets are not real credentials.
- Parent reported cache/socket sandbox restrictions and a rerun with escalation, plus passed frontend gates. That is parent-supplied evidence, not independently verified here. This reviewer requested escalation for narrow tests directly; there was no local environment-failure test result to misclassify as a defect.
- All test exit codes below are tool-returned process exit codes, not inferred through a pipe. Passing reproductions intentionally assert the observed defective behavior; they are not evidence that the invariant holds.

## Role-resource matrix

Admin API prefix below is `/admin/api/v1`. `S` means a valid session, with required-MFA enrollment enforced on ordinary Admin routes; `C` means same-origin plus session-derived CSRF. Admin roles are instance-wide, not project-scoped.

| Resource / action | Anonymous or Gateway Key alone | read_only + S | administrator + S | Additional boundary / evidence |
| --- | --- | --- | --- | --- |
| UI/static shell, setup status, UI bootstrap, health | Available | Available | Available | Public bootstrap subset; `runtime.go:1562-1574` |
| First Admin creation | Conditional | Not a post-setup operation | Not a post-setup operation | Same-origin, first-user atomic create, token for nonlocal/external-origin setup; `admin_setup.go:69`, `:161` |
| Password login / MFA challenge completion | Credentials/challenge required | Same | Same | Login source limit; claimed challenge; generation and factor validation |
| Session read, own MFA status | Denied without cookie | Own account | Own account | Base-session routes permit enrollment before required-MFA completion |
| Logout, initial MFA create/confirm/cancel pending | Denied | Own account + C | Own account + C | Setup-safe self wrappers; no arbitrary username input |
| Own password, MFA management, preferences | Denied | Own account + C | Own account + C | Intentional exception to literal “GET only”; password/factor checks depend on action; SEC-02/03 |
| Dashboard, usage, configuration, projects/keys, credentials, providers/profiles/catalog, deployments/prices/proposals, routes, policies, alerts, audit, runbooks, Admin-user list | Denied | Read all projects | Read all projects | `runtime.go:1586-1696`; metadata views exclude password hashes, credential ciphertext and Gateway-key hashes (`admin_resources.go:18-46`, `admin_users.go:27-45`) |
| Failure payload GET | Denied | Read retained customer content | Same | Capture must exist; project resolved internally; AEAD scope; audit before body; C2 and SEC-04 |
| System resource POST/PUT/PATCH/DELETE, developer execute, refresh/test/probe endpoints | Denied | Denied | Allowed + C | Default role-gated mutation wrappers; actual route sweep passed |
| Destructive operations / protection edits | Denied | Denied | Above plus handler step-up | Password and active TOTP; session-scoped finite elevation for eligible operations. Account create/delete always reauthenticate (`admin_users.go:14-24`) |
| Master-key custody/runbooks | Denied | Metadata only | Metadata only | No online Admin rotate/rewrap/revoke route registered; actual key mutation is offline CLI plus data lock |
| Gateway calls / resource IDs | Gateway Key required | Admin cookie gives no Gateway grant | Same | Key/project enabled, expiry, tombstone checks in `auth/snapshot.go`; Files owner lookup uses authenticated project (`gateway/inference_resources_store.go:419`) |
| Same-project different Gateway Key | Authenticated project ownership for reviewed Files path | N/A | N/A | Key-specific isolation must not be assumed. Deferred execution rechecks recorded Key ID (`gateway/deferred_response.go:711`); full resource matrix belongs to protocol/core review |
| Different-project Gateway Key | Refused by reviewed project-keyed resource lookup | N/A | N/A | No Admin authority inferred from a Gateway bearer |
| `/metrics`, `/audit/anchors` | Separate bearer domains, subject to metrics RequireAuth configuration | Admin cookie alone insufficient when auth enabled | Same | `metrics.go:39`, `audit_anchor.go:111`; anchor authorizer required and separate from metrics credentials |

Server-side role is reread per request (`admin_session.go:286`). Downgrade therefore affects subsequent requests, not requests already past authorization. Cookie is `__Host-halro_session`, Secure/HttpOnly/SameSite=Strict (`admin_session.go:364`); mutations also check origin and CSRF. `runtime.go:1703` sets no-store, restrictive CSP, no-referrer and nosniff. Browser behavior was not exercised here.

## Findings

### SEC-01 — Master-key rotation leaves retained ciphertext under the old key

- **Type/status:** confirmed defect in file-mode reproduction; KMS extension source-confirmed, runtime reproduction outstanding. **P1 / high confidence.** Owners proposed: key lifecycle + retained resource/failure-capture owners. INV-07/09. Independent adjudication pending.
- **Locations:** `internal/app/key_rotation.go:257-297` rewrites metadata credentials/MFA/system envelopes only; `:433` begins verification of the same metadata set. `internal/app/kms_key_lifecycle.go:975-1014` has the same omission. `internal/vault/vault.go:108`, `:127`, `:190` derive capture/resource encryption directly from the Master Key. Runtime passes that Vault to both stores at `internal/app/runtime.go:446` and `:472`.
- **Reachable trigger:** create a retained failure capture, uploaded/local file, batch/local result, or deferred input/output before an authorized offline DEK rotation; rotate successfully and read it with the new runtime. No attacker or crash is required.
- **Observed:** `TestSecurityReviewRotationRetainedCiphertext` writes a real capture and correctly scoped provider-object ciphertext in the fixture data directory. `RotateMasterKey` succeeds. Capture read then returns `open failure capture: secret authentication failed`; object bytes are unchanged, new Vault rejects them, old Vault still opens them.
- **Serving impact:** failure payload handler maps open failure to 404 (`app/failure_capture.go:128-136`). Local file download reads the sealed object and maps failure to 503 (`gateway/inference_resources_store.go:535-560`); deferred execution reads the input object at `gateway/deferred_response.go:520`. Other resource effects follow those common readers; not all were executed. The test object was a cryptographic fixture, not a registered uploaded resource.
- **Expected:** file runbook explicitly promises “rewrites all Vault ciphertext” (`docs/runbooks/file-master-key-rotation.md:6-8`), and rotation must preserve readable retained objects or explicitly refuse until safely handled.
- **Existing defenses:** offline lock, copy-on-write metadata, compaction, candidate key check, preserved Audit/Ledger HMAC envelopes, MFA rewrite, session invalidation and old→new recovery bridge all work for their covered metadata. They never enumerate/rewrite these external ciphertext directories. The bridge does not provide new→old reads and is removed on completion. Startup only recognizes envelope headers for legacy cleanup; it cannot migrate correctly sealed old-key objects.
- **Severity boundary:** important retained workflows become unavailable after a successful maintenance operation. This is not demonstrated global irreversible data loss: retaining the old key/full backup provides recovery material. Discarding it can make recovery impossible, but that was not performed.
- **Minimal direction (proposal only):** include every retained object class in a crash-safe generation migration, or introduce separately wrapped stable object keys; as an interim measure refuse rotation with live dependent objects and explain the condition. Regression must cover live file, deferred input/output, capture, file and KMS modes, each publish interruption, restart and old-key retirement. Existing metadata-only rotation tests pass and miss this class.

### SEC-02 — In-flight idle refresh resurrects a revoked Admin session

- **Type/status:** confirmed deterministic concurrency defect. **P1 / high confidence.** Owner proposed: adminauth/store. INV-04. Independent adjudication pending.
- **Locations:** `internal/adminauth/session.go:103-126` reads/validates then unconditionally saves refresh; `:147-151` revokes by delete. `internal/store/bolt/store_admin.go:193-195` is an unconditional upsert. Logout calls Revoke at `internal/app/admin_session.go:169` and clears cookie/elevation, but does not change user generation.
- **Reachable trigger:** an Admin request with an idle-refresh-due session reads the row; concurrent logout deletes it; delayed refresh commits afterward. Another party retaining that token can use it after logout. This also happens naturally with overlapping tabs; exploitation requires possession of the prior valid cookie.
- **Reproduction:** `TestSecurityReviewRefreshResurrectsRevokedSession` wraps the actual bbolt store to pause exactly before refresh persistence, verifies that Revoke deleted the row, resumes the write, then successfully authenticates the old token on a later call. `go test -race` passes while logging `err=<nil>` for authentication after completed revoke.
- **Existing defenses:** random/hash-only session tokens, idle/absolute expiry, user generation checks and current-role lookup. All remain valid because ordinary logout does not bump generation. Browser cookie deletion cannot revoke a copied token; clearing elevation limits destructive operations but does not remove ordinary session authority. Password/MFA identity rotation *does* bump generation and is not shown bypassed here.
- **Impact:** successful logout does not reliably terminate future use of the server-side session; retained access lasts subject to normal absolute expiry/current-role rules. The reproduction is at the exact production session/store boundary, not an HTTP stress demonstration.
- **Minimal direction (proposal only):** use an atomic refresh-if-present operation with generation/expiry validation in the same transaction, and prevent refresh from creating a missing row. Add deterministic logout/refresh and identity-rotation/refresh tests, including same-token request concurrency and restart persistence.

### SEC-03 — Management MFA failures bypass the account failure budget and audit

- **Type/status:** confirmed through Admin router. **P2 / high confidence.** Owner proposed: Admin MFA. INV-04/08.
- **Locations:** `internal/app/admin_mfa.go:101-156` (add authenticator), `:529-540` (recovery regeneration), `:585-607` (disable); delete-authenticator has the same split at `:450-483`. `admin_stepup.go:guardAdminCredentialCheck` records only failures returned from its callback.
- **Reachable trigger:** valid enrolled session + CSRF/origin + correct current password, but repeatedly wrong six-digit TOTP. The guard callback validates password only; the subsequent TOTP/recovery rejection never calls `recordStepUpFailure` or `auditStepUp`.
- **Observed:** seven wrong six-digit codes per endpoint (create authenticator, regenerate recovery codes, disable optional MFA), all HTTP 401, no 429 and zero `admin.reauthentication` failure records. Test pins the account-budget clock and chooses a code different from the current ±1 TOTP steps. No valid factor or new credential was obtained.
- **Existing defenses:** valid cookie and CSRF, required-MFA gate, Argon2 cost/concurrency cap, single-use accepted TOTP steps, factor expiry, and strong recovery-code entropy. Anonymous login MFA uses claimed challenges and its own carried-forward attempts; that route is separately guarded and is not bypassed by this reproduction. `mfa_policy=required` blocks disable entirely, but does not fix add/regenerate paths. Wrong-password management requests are correctly budgeted.
- **Impact/severity:** an attacker who already has a session and password can keep guessing the factor protecting persistent account changes without the intended account budget or failure trail. This is a constrained second-factor hardening failure, not anonymous MFA bypass or proven account takeover.
- **Minimal direction (proposal only):** place the complete action-specific password/factor verification inside the failure guard, preserving the “other authenticator” and recovery-code semantics; account for invalid second factors on every management path. Regression should cover each factor path, shared budget, correct-password/wrong-factor attempts, concurrent guesses and failure audit counts. Existing credential-guard test checks wrong passwords, not this shape.

### SEC-04 — Failure-capture reads do not enforce expiry

- **Type/status:** confirmed store-level privacy defect, HTTP reachability source-traced. **P2 / high confidence.** Owner proposed: failurecapture + runtime maintenance. INV-07.
- **Locations:** `internal/failurecapture/failurecapture.go:387-433` decrypts/returns without checking CapturedAt; `:484-502` lookup discards the timestamp. `internal/app/failure_capture.go:114-161` serves that result. Purge runs after export on `runtime.go:961-975` and graceful shutdown `:990`; `config/default.go:65` defaults the export interval to one hour.
- **Trigger/observed:** capture with one-hour retention; move injected clock two hours forward, read before purge. `TestSecurityReviewExpiredCaptureRead` gets `found=true, err=nil`. Calling Purge then removes it. Open itself does not purge (`failurecapture.go:175-202`).
- **Existing defenses:** authenticated audited read, AEAD request/project binding, configured retention, purge even when capture is disabled, and graceful-shutdown purge. These constrain access/cleanup but cannot enforce an expiry deadline on each read. Healthy operation has a sweep-delay window; slow export, failed deletion or repeated hard restarts may extend it. No claim of a universal one-hour upper bound.
- **Expected:** INV-07 explicitly excludes over-expiry readable retention objects; the code describes retention as the actual window. This is separate from C2: even an authorized Administrator should not receive an expired payload under that promise.
- **Direction:** reject expired captures in lookup/read using trusted capture time, independent of physical cleanup; add expiry-at-boundary and read-during-purge-failure tests. A periodic deletion SLA may be separately documented, but should not silently define content access TTL.

### SEC-05 — Audit pagination materializes the entire history under the append mutex

- **Type/status:** source-confirmed boundedness defect; capacity impact not experimentally measured. **P2 / medium confidence on operational severity.** Owner proposed: audit/Admin resources. INV-08.
- **Locations/trigger:** authenticated `GET /admin/api/v1/audit?limit=1` (`runtime.go:1696`) calls `admin_resources.go:324-350`: Replay appends every matching event into a slice, reverses the entire slice, then truncates to limit. `audit/log.go:243-254` holds the same mutex used by AppendBatch while scanning the whole file. Cursor 0 matches all history.
- **Impact:** response pagination bounds serialization only. Each request consumes O(history) decoding/memory and blocks audit appends for the scan; concurrent read-only sessions can repeat it. Log history is not bounded by the 4096-event dedup index.
- **Existing defenses:** authentication, page-size parser, authenticated audit frames and per-frame 64 KiB ceiling; none bounds total query work. No anonymous route or measured OOM claim.
- **Direction:** retain at most the requested page during a scan, add an index/seek strategy if needed, and avoid holding the append lock across historical decoding. Measure audit append latency and peak memory with a large fixture and repeated limit=1 requests. Do not call this an experimentally confirmed denial of service yet.

## C2 contract dispute and rejected interpretations

`internal/domain/admin.go:9-14` deliberately defines two roles and GET-only read access without per-endpoint exceptions. Current source and route registration permit read_only to fetch failure content; `internal/app/failure_capture.go:80-95` explicitly describes this content read as audited. Historical `docs/review/260903/progress.md:87-94` already distinguishes this from a bypass and leaves it as a product decision. The historical conclusion was not taken as current runtime evidence.

Current evidence: route remains `requireAdmin` at `runtime.go:1600`; no administrator-only gate exists in the payload handler; failed audit returns 503 before body. `TestAPayloadReadIsRefusedWhenItCannotBeAudited` passed in this review. Scope binding prevents substitution, not authorized instance-wide Admin reads. This reviewer did not repeat an actual read_only payload download.

**Disposition: unresolved contract dispute, no new vulnerability severity assigned.** Product must choose either explicitly content-authorized read_only semantics or a distinct content-read permission/administrator gate and consistent UI/docs. Default-disabled capture reduces exposure but does not decide who should be authorized after enabling it. The self-account mutation exceptions also mean “GET only” is shorthand rather than the literal route contract.

Other interpretations not promoted to vulnerabilities:

- `masterkey.NewUnlocker` rejects key_slots, but app key-slot unlocking has its own implemented trusted KMS path; this is not evidence KMS is unavailable.
- A provider/client library accepting arbitrary URLs is not sufficient SSRF evidence: actual Admin creation and runtime loading call ValidateURL, and the pinned dialer rejects every forbidden DNS answer, disables env proxies and refuses redirects.
- Builtin contentscan is explicitly a format-admission gate, not antivirus. No malware-detection capability was assumed.
- Audit-anchor emission is explicitly availability-oriented and fail-open (`audit_anchor.go:22-26`); that is different from the fail-closed payload read and durable mutation audit intents.
- Go heap copies are not claimed fully zeroized: Vault clears owned slices, but AES key schedules, JSON/net/http buffers and SDK immutable strings are not a verified secure allocator. Host hardening records this limitation.

## Coverage and lifecycle review

| Area | Source coverage / reviewed path | Evidence status and limits |
| --- | --- | --- |
| Gateway auth | `auth/gatewaykey.go`, `auth/snapshot.go`; format/randomness/hash, disabled/expired/deleted projects/keys, refresh ordering, queued Key ID auth | Selected key/snapshot tests passed; full project/endpoint resource matrix deferred to parent/core |
| Admin auth | all production `adminauth` files; `app/admin_session.go`, `admin_setup.go`, `admin_mfa.go`, `admin_stepup.go`, `admin_users.go`; actual full Admin route registration; `store/bolt/store_admin.go` session, challenge, factor and identity transactions | Role sweep, required-MFA and factor-consumption defenses passed; SEC-02/03 |
| Host/outbound | all platform production `hostsecurity` files; complete `safetransport/transport.go`; provider binding client, endpoint loader/create validation, webhook policy/client, tokenless setup host pin | DNS, pinning, redirect/proxy and reserved-range tests passed. Linux prctl source-only on Darwin; no real network metadata probe |
| Vault/file keys | all production Vault files; `masterkey/masterkey.go`; `app/key_rotation.go`, `vault_material.go`; direct retention readers/writers and runtime sealer wiring | Credential binding/tamper/symlink and metadata rotation test passed; SEC-01. Full crash matrix not rerun |
| Key slots/KMS | `masterkey/slots.go`, audit intent types; `kms/contract.go`, `payload.go`, `retry.go`; AWS adapter/options/context/error mapping; `app/kms_master_key.go` trusted binding/unwrapper, `kms_key_lifecycle.go` rewrap/revoke/rotate/finalize and durable audit delivery | Selected state-machine/retry/AWS mock tests passed. No IAM/CloudTrail/live recovery acceptance. Initialization and disaster-recovery integration not exhaustively exercised |
| Bearer credentials | complete `bearercred/credentials.go`, `lock_unix.go`; metrics and anchor authorization entry points | Overlap/revocation/restore and bad-state/permissions tests passed. Credential file/audit rely on trusted local filesystem; unkeyed local hash chain is not external-witness protection |
| Audit | complete `audit/log.go`; payload read, Admin durable-intent protocol, MFA intent handling, audit anchors/checkpoint consumers, Admin list | Tamper/wrong-key/partial-tail tests and payload withholding passed; SEC-05. Disk-full append retry/poisoning and long-lived dedup eviction not fault-injected |
| Content/privacy | complete `contentscan/scanner.go`; redaction engine/inspect/stream/jsonescape; mandatory patterns, recursive values/member names/numbers, tool escape mirror, streaming suffix and byte ceilings; gateway policy admission and app policy loading | Selected bounded-stream and format tests passed. Full native-protocol stream/end-to-end redaction is not established by these tests |
| Token Guard | `tokenguard/manager.go`: per-subject locks, admission/lease release, fixed thresholds, EWMA taint/freeze, checkpoint validation/restore, events and pruning; `app/tokenguard.go` loader | Atomic lease, block restore and missing-policy tests passed. No quantitative multi-source flood/long-run state bound measurement |
| Safe logs | complete `safelog/safelog.go`, structured-attribute shape tests, KMS error wrapping | Shape and unknown-format-sensitive-name tests passed. Pattern masking is not proof arbitrary text contains no secrets |

Lifecycle summary: Gateway keys are generated and persisted as hashes; Admin password hashes use Argon2id and session cookies are hash-indexed; accepted TOTP steps and recovery-code use are serialized by bbolt transactions; MFA identity changes rotate session generation and carry audit intents. Vault uses random GCM nonces, length-delimited AAD, scope-specific HKDF and closed-Vault checks. File keys require 32-byte regular non-symlink files without group/other permissions. KMS binds instance/slot plus purpose, exact trusted key/algorithm and candidate Vault identity; production readiness requires active verified primary and recovery slots. Retiring/revoked descriptors retain nonsecret evidence and compact revoked wrapping material. None of those metadata lifecycle protections currently migrates the externally stored objects in SEC-01.

## Tests mapped to invariants and commands

All commands ran from `/Users/ziy/Code/ClayCosmos/Halro`, using escalated cache/local-test access. Every command below exited **0**. No real-provider smoke was selected. Test-reported elapsed times omit some compilation/tool overhead.

| Evidence | Command selector / invariant | Result / log |
| --- | --- | --- |
| E0 exploratory MFA reproduction | App overlay, only `TestSecurityReviewWrongManagementTOTPBudget`; malformed code exploratory version | Confirmed no audit/throttle; 4.906s; `mfa-repro.log`. Superseded by E1 six-digit reproduction |
| E1 defect reproductions | App overlay, `^TestSecurityReview`; INV-04/07/09 | All three reproduced; 5.038s; `reproductions.log` |
| E2 revocation interleaving | adminauth overlay, `-race`, exact session test; INV-04 | Revoked session accepted again; 2.881s; `session-repro.log` |
| E3 Admin defenses | Exact five tests below; INV-04/07/09/10 | All passed; 4.948s; `admin-defenses.log` |
| E4 primitive defenses | Exact selectors below; INV-04/07/08/09 plus SSRF | All selected tests passed; per-package 0.416–3.682s; `primitive-defenses.log`. safelog had **no selected tests** in E4; actual safelog evidence is E5 |
| E5 audit/bearer/log/KMS defenses | Exact selectors below; INV-07/09, credential privacy | All selected tests passed; `additional-defenses.log` |

```sh
go test -count=1 -v -overlay /private/tmp/halro-security-review-260905/overlay.json ./internal/app -run '^TestSecurityReviewWrongManagementTOTPBudget$' > /private/tmp/halro-security-review-260905/mfa-repro.log 2>&1

go test -count=1 -v -overlay /private/tmp/halro-security-review-260905/overlay.json ./internal/app -run '^TestSecurityReview' > /private/tmp/halro-security-review-260905/reproductions.log 2>&1

go test -race -count=1 -v -overlay /private/tmp/halro-security-review-260905/session-overlay.json ./internal/adminauth -run '^TestSecurityReviewRefreshResurrectsRevokedSession$' > /private/tmp/halro-security-review-260905/session-repro.log 2>&1

go test -count=1 -v ./internal/app -run '^(TestReadOnlyRoleCannotReachAnyRegisteredMutationRoute|TestRequiredMFAPolicyRestrictsUnenrolledSessionToSetup|TestAdminMFALoginRequiresAndConsumesSecondFactor|TestAPayloadReadIsRefusedWhenItCannotBeAudited|TestMasterKeyRotationReencryptsCredentialsAndPreservesAuditChain)$' > /private/tmp/halro-security-review-260905/admin-defenses.log 2>&1

go test -count=1 -v ./internal/safetransport ./internal/auth ./internal/vault ./internal/redaction ./internal/tokenguard ./internal/masterkey ./internal/kms ./internal/contentscan ./internal/safelog -run '^(TestMixedPublicPrivateDNSAnswerIsRejectedBeforeDial|TestDialUsesValidatedIP|TestClientIgnoresEnvironmentProxyAndRefusesRedirects|TestReservedAndTunnelAddressesAreRefused|TestStaleRefreshCannotResurrectARevokedKey|TestSnapshotAuthentication|TestCredentialRoundTripAndAudienceBinding|TestCredentialTamperFails|TestMasterKeyRejectsSymlink|TestCompilePolicyRejectsUnboundedStreamingEnforcement|TestBoundedStreamingUsesRollingBuffer|TestAcquireCountsConcurrencyAtomicallyAndReleaseIsIdempotent|TestCheckpointV2RestoresLiveBlockAndV1Baseline|TestAMissingPolicyRefusesInsteadOfAdmittingUnconditionally|TestKeySlotVerificationFailsClosed|TestKeySlotTransitionsEnforceRevisionsAndRecoveryInvariants|TestExecutorFailsFastForPermanentErrors|TestBuiltinRejectsExecutablesArchivesAndInvalidText)$' > /private/tmp/halro-security-review-260905/primitive-defenses.log 2>&1

go test -count=1 -v ./internal/safelog ./internal/audit ./internal/bearercred ./internal/kms/awskms -run '^(TestNoAttributeShapeCarriesACredentialToTheHandler|TestUnrecognisedCredentialFormatsAreCaughtByAttributeName|TestAuditDetectsTamperingAndWrongKey|TestOpenTruncatesOnlyPartialAuditTail|TestRotateOverlapRevokeAndRestore|TestRejectsBroadPermissionsAndCorruptState|TestAdapterRejectsRequestsOutsideAllowlistBeforeAWSCall|TestAWSErrorMappingAndSecretSafeOutput)$' > /private/tmp/halro-security-review-260905/additional-defenses.log 2>&1
```

Raw-log SHA-256:

| Log | SHA-256 |
| --- | --- |
| reproductions.log | `42e0ba28c3408cdb59a7bc76e0ea84272cb7d4f84d2dbe00fa18f5b2747faf25` |
| session-repro.log | `203ac945697ce223a2aa1167e78d1e3d8106ec331b9732c2a01f0b526ff87d66` |
| admin-defenses.log | `d7b949f443e9b4d8258f543f4b08ff5e9d266f1a5449111cc3f58ec36e32e628` |
| primitive-defenses.log | `de569bb25caf5197a7cf8de1f8f84e8c3e8a6815f77c7035a6c687f36d9cd21f` |
| additional-defenses.log | `c4364202a69c1047ac24ca0af1ef87b23cd2a7224924d917bca628035404530b` |

## Remaining gaps and next evidence

1. Independent reviewer must try to refute SEC-01/02 against the exact entry points and defenses. Neither is marked independently adjudicated by this author. Extend SEC-01 to registered Files/deferred resources and fake-KMS DEK rotation before closing the finding.
2. Add management wrong-factor tests for delete-authenticator and enrollment confirmation; concurrent failure-budget reservation is also untested here (budget check and failure recording are separate critical sections). Do not assume the tested sequential budget proves a strict concurrent bound.
3. Add expired-capture HTTP reads with purge delayed/failed, disabled capture, restart and exact TTL boundary. Existing purge tests do not prove access-time expiry.
4. Audit failure-after-partial-write behavior needs deterministic fault injection: AppendBatch returns before advancing in-memory head on write/sync failure, while later app calls may retry. This was noticed during source reading but no fault-trigger reproduction was completed; it is an open test gap, not an additional confirmed finding.
5. Stream tests should cover aggregate choice/tool/channel counts, UTF-8/surrogate/escape splits, malformed trailing JSON and policy lifecycle at real protocol entry points. Per-channel byte caps alone do not prove total buffering bounds. No new fuzz campaign was run.
6. Verify full key-slot initialization/recovery/publication interruption matrix, Linux process-hardening controls, actual cloud identities/endpoint policy, and external witness restoration separately. Local mocks cannot establish those deployment facts.
7. No findings here certify INV-01/02/03/05/06 globally; parent/core/protocol reviewers own those proofs. INV-10 here covers server route roles, not UI interaction. No modification, commit, push, release or real-provider call was made.

## Reproduction sources (preserved in this report)

The following are test-only overlay files. Recreate them under `/private/tmp/halro-security-review-260905/`, and map the corresponding nonexistent package test paths through Go's `Replace` overlay. Existing fixture helpers come from the baseline test tree. No production file replacement is required.

### overlay.json

```json
{"Replace": {"/Users/ziy/Code/ClayCosmos/Halro/internal/app/security_review_test.go": "/private/tmp/halro-security-review-260905/security_review_test.go"}}
```

### session-overlay.json

```json
{"Replace": {"/Users/ziy/Code/ClayCosmos/Halro/internal/adminauth/session_review_test.go": "/private/tmp/halro-security-review-260905/session_review_test.go"}}
```

### security_review_test.go

```go
package app
import (
 "context"
 "os"
 "path/filepath"
 "bytes"
 "github.com/akz142857/Halro/internal/vault"
 "github.com/akz142857/Halro/internal/failurecapture"
 "github.com/akz142857/Halro/internal/adminauth"
 "net/http"
 "net/http/httptest"
 "testing"
 "time"
 "github.com/akz142857/Halro/internal/domain"
 "github.com/akz142857/Halro/internal/audit"
)
func TestSecurityReviewWrongManagementTOTPBudget(t *testing.T) {
 for _, endpoint := range []string{"/admin/api/v1/security/mfa/authenticators", "/admin/api/v1/security/mfa/recovery-codes/regenerate", "/admin/api/v1/security/mfa"} {
 t.Run(endpoint, func(t *testing.T) {
 r, session := stepUpTestRuntime(t)
 secret := []byte("12345678901234567890")
 ciphertext, err := r.vault.EncryptAdminMFA("mfa_review", "admin", secret); if err != nil {t.Fatal(err)}
 now:=time.Now().UTC()
 _,err=r.store.PutAdminMFAAuthenticator(context.Background(),domain.AdminMFAAuthenticator{ID:"mfa_review",Username:"admin",Name:"review",Type:domain.AdminMFATypeTOTP,SecretCiphertext:ciphertext,Status:domain.AdminMFAStatusActive,CreatedAt:now,ConfirmedAt:&now},0);if err != nil {t.Fatal(err)}
 wrong := "000000"
 for { valid:=false;for _,offset:=range []int64{-1,0,1} {if wrong==adminauth.TOTPCode(secret,time.Now().Unix()/30+offset) {valid=true}};if !valid {break};wrong="111111" }
 for i:=0;i<7;i++ {
 method:=http.MethodPost;if endpoint=="/admin/api/v1/security/mfa" {method=http.MethodDelete}
 body:=map[string]string{"current_password":stepUpTestPassword,"code":wrong};if endpoint=="/admin/api/v1/security/mfa/authenticators" {body["name"]="extra"}
 req:=adminMutationRequest(t,method,endpoint,session,body)
 res:=httptest.NewRecorder();r.adminRouter().ServeHTTP(res,req)
 t.Logf("attempt=%d status=%d",i+1,res.Code)
 if res.Code!=http.StatusUnauthorized {t.Fatalf("unexpected status=%d body=%s",res.Code,res.Body.String())}
 }
 failures:=0
 _,err=r.audit.Replay(func(rec audit.Record)error{if rec.Event.Action=="admin.reauthentication"&&rec.Event.Outcome=="failure" {failures++};return nil});if err!=nil {t.Fatal(err)}
 t.Logf("failure audits=%d",failures)
 if failures!=0 {t.Fatalf("expected reproduced missing audit; got %d",failures)}
 })
 }
}

func TestSecurityReviewRotationRetainedCiphertext(t *testing.T) {
 cfg, newFile, _, _, oldKey, oldAudit := rotationFixture(t);defer clear(oldKey);defer clear(oldAudit)
 oldVault,err:=vault.New(oldKey);if err!=nil {t.Fatal(err)};defer oldVault.Close()
 options:=failurecapture.Options{Root:filepath.Join(cfg.Storage.DataDir,"failures"),MaxBytes:4096,MaxRecordsPerDay:10,Retain:time.Hour}
 captures,err:=failurecapture.Open(oldVault,options);if err!=nil {t.Fatal(err)}
 if ok,err:=captures.Put(failurecapture.Record{RequestID:"req_review",ProjectID:"prj_review",Outcome:"failed",Request:[]byte(`{"text":"review fixture"}`)});err!=nil||!ok {t.Fatalf("put=%v %v",ok,err)}
 if _,found,err:=captures.Get("req_review","prj_review");err!=nil||!found {t.Fatalf("before=%v %v",found,err)}
 sealed,err:=oldVault.EncryptResourceObject("file_review:content","prj_review",[]byte("review fixture"));if err!=nil {t.Fatal(err)}
 directory:=filepath.Join(cfg.Storage.DataDir,"provider-objects");if err=os.MkdirAll(directory,0700);err!=nil {t.Fatal(err)}
 path:=filepath.Join(directory,"file_review.content");if err=os.WriteFile(path,sealed,0600);err!=nil {t.Fatal(err)}
 if _,err:=RotateMasterKey(context.Background(),cfg,newFile);err!=nil {t.Fatal(err)}
 newKey,err:=vault.LoadMasterKey(cfg.Storage.MasterKey.File);if err!=nil {t.Fatal(err)};defer clear(newKey)
 next,err:=vault.New(newKey);if err!=nil {t.Fatal(err)};defer next.Close()
 after,err:=failurecapture.Open(next,options);if err!=nil {t.Fatal(err)}
 _,found,err:=after.Get("req_review","prj_review");t.Logf("after successful rotation: capture found=%v err=%v",found,err);if err==nil {t.Fatal("expected reproduction: capture unreadable")}
 raw,err:=os.ReadFile(path);if err!=nil {t.Fatal(err)};if !bytes.Equal(raw,sealed) {t.Fatal("rotation changed object")}
 _,err=next.DecryptResourceObject("file_review:content","prj_review",raw);t.Logf("unchanged provider object: new key read err=%v",err);if err==nil {t.Fatal("expected reproduction: object unreadable")}
 if _,err=oldVault.DecryptResourceObject("file_review:content","prj_review",raw);err!=nil {t.Fatal("old key should still read fixture")}
}
func TestSecurityReviewExpiredCaptureRead(t *testing.T) {
 now:=time.Date(2026,9,5,0,0,0,0,time.UTC)
 v,err:=vault.New(bytes.Repeat([]byte{1},32));if err!=nil {t.Fatal(err)};defer v.Close()
 s,err:=failurecapture.Open(v,failurecapture.Options{Root:t.TempDir(),MaxBytes:4096,MaxRecordsPerDay:10,Retain:time.Hour,Now:func()time.Time{return now}});if err!=nil {t.Fatal(err)}
 if ok,err:=s.Put(failurecapture.Record{RequestID:"req_review",ProjectID:"prj_review",Request:[]byte(`{"text":"review"}`)});err!=nil||!ok {t.Fatal(err)}
 now=now.Add(2*time.Hour)
 _,found,err:=s.Get("req_review","prj_review");t.Logf("two hours later with one-hour retention: found=%v err=%v",found,err);if err!=nil||!found {t.Fatal("expected reproduction: readable before purge")}
 if err=s.Purge();err!=nil {t.Fatal(err)}
 _,found,err=s.Get("req_review","prj_review");if err!=nil||found {t.Fatal("purge should enforce expiry")}
}
```

### session_review_test.go

```go
package adminauth
import (
 "context"
 "path/filepath"
 "testing"
 "time"
 "github.com/akz142857/Halro/internal/domain"
 boltstore "github.com/akz142857/Halro/internal/store/bolt"
)
type reviewRefreshStore struct { *boltstore.Store; entered, resume chan struct{} }
func (s *reviewRefreshStore) PutAdminSession(ctx context.Context, value domain.AdminSession) error {
 if !value.LastSeenAt.Equal(value.CreatedAt) {close(s.entered);<-s.resume}
 return s.Store.PutAdminSession(ctx,value)
}
func TestSecurityReviewRefreshResurrectsRevokedSession(t *testing.T) {
 db,err:=boltstore.Open(filepath.Join(t.TempDir(),"metadata.db"));if err!=nil {t.Fatal(err)};defer db.Close()
 now:=time.Date(2026,9,5,0,0,0,0,time.UTC)
 user,err:=NewUser("admin",[]byte("review passphrase"),domain.AdminRoleAdministrator,now);if err!=nil {t.Fatal(err)}
 user,err=db.PutAdminUser(context.Background(),user,0);if err!=nil {t.Fatal(err)}
 store:=&reviewRefreshStore{Store:db,entered:make(chan struct{}),resume:make(chan struct{})}
 manager,err:=NewManager(store,make([]byte,32),time.Hour,10*time.Minute);if err!=nil {t.Fatal(err)};defer manager.Close()
 created,err:=manager.Create(context.Background(),user,now);if err!=nil {t.Fatal(err)}
 done:=make(chan error,1);go func(){_,err:=manager.Authenticate(context.Background(),created.Token,now.Add(time.Minute));done<-err}()
 <-store.entered
 if err:=manager.Revoke(context.Background(),created.Token);err!=nil {t.Fatal(err)}
 if _,err:=db.GetAdminSession(context.Background(),created.Session.IDHash);err==nil {t.Fatal("revoke did not delete")}
 close(store.resume);if err:=<-done;err!=nil {t.Fatal(err)}
 _,err=manager.Authenticate(context.Background(),created.Token,now.Add(time.Minute+time.Second));t.Logf("authenticate after completed revoke and delayed refresh: err=%v",err)
 if err!=nil {t.Fatal("expected reproduction: revoked token becomes valid again")}
}
```
