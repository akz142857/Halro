# SEC-01 independent rotation adjudication

Baseline: `381743f6613607dc256828f4776b52af8bdd232c`, verified 2026-09-05. Read AGENTS.md, the approved review plan, and the expressly authorized `roles/security.md`. This adjudication modifies only this report; synthetic fixtures and logs are under `/private/tmp/halro-security-rotation-adjudication-260905`. No production source, repository tests, git state, or user data changed. No full gates, paid providers, real KMS, or runtime backup drill.

## Verdict

**CONFIRMED — P1, high confidence**, for replacing the file Master Key or rotating the KMS-backed Master Key/DEK while unexpired sealed failure captures or local provider/deferred objects remain. Both actual rotation implementations complete successfully without migrating these objects; the active new key cannot authenticate them. This is a reachable retained-data availability defect, and incomplete live-ciphertext retirement during compromise response. It is not evidence of plaintext exposure, cross-tenant disclosure, or unavoidable permanent loss.

**REFUTED as a blanket claim about every KMS rotation:** KEK rewrap keeps the DEK unchanged and does not have this failure mechanism. Real AWS behavior is unverified; fake-KMS evidence exercises the actual Halro DEK lifecycle code, not AWS production acceptance.

P1 is justified because an authorized, apparently successful security-maintenance operation breaks still-retained caller results and queued requests without a migration or preflight refusal. Deferred results have no upstream twin. P0 is not justified: impact requires retained local ciphertext and a changed DEK; live inference and migrated credentials need not fail, and old-key custody plus appropriate pre-rotation backups can provide recovery. Failure captures alone would have a smaller operational impact than the combined deferred/result path.

## Independent causal trace and reachable triggers

| Stage / source | Current evidence |
| --- | --- |
| `internal/app/key_rotation.go:257`, `:433` | File rotation rewrites credential/MFA/system metadata and verifies those same classes. External retained directories are outside the rewrite/verification. Publication replaces the active key, then clears the temporary bridge. |
| `internal/app/kms_key_lifecycle.go:975`, `:1036`, `:1081` | DEK rotation performs the analogous metadata rewrite, verifies new key slots and metadata, publishes, unlocks the new primary DEK, and finalizes. No retained-object rewrite. |
| `internal/masterkey/slots.go:139` | New DEK descriptor starts a new generation with an empty slot list subsequently populated with new-DEK wrappers. This is not a retained old-DEK reader ring. |
| `internal/vault/vault.go:108`, `:127`, `:182` | Failure and resource AEAD keys derive directly from the sole master key via HKDF with kind/id/project scope. The HVLT envelope contains format version and nonce, not an independently wrapped object key or selectable master-key generation. Changing the master key changes both scoped keys. |
| `internal/app/runtime.go:446`, `:472` | Restart wires the active Vault into failure capture and `ResourceObjectSealer`. There is no runtime old-key fallback. |
| `internal/gateway/inference_resources_store.go:147`, `:199` | Objects are sealed to disk using resource ID plus `content`/`input` role and project; reads authenticate against that same scope using the current sealer. Metadata retention of a filename does not retain its key. |
| `internal/failurecapture/failurecapture.go:387`; `internal/app/failure_capture.go:121` | A previously readable capture returns authentication failure after rotation. Admin retrieval intentionally masks it as 404. Capture-disabled configuration still opens an existing directory (`failure_capture.go:24`); disabling future capture is not migration. |
| `internal/gateway/deferred_response.go:442`, `:520`, `:773`, `:969` | Deferred input and completed output use the affected object store. A resumed queued record with valid ownership/target resolution fails before upstream execution when input cannot open (`deferred_response_unreadable`). Completed answer retrieval returns 503 (`resource_store_unavailable`). |
| `internal/gateway/inference_resources_store.go:545` | A file with a local ObjectPath encounters the same failure on download, reported as 503. Files without a local ObjectPath use upstream retrieval and are not affected by this local-object mechanism. |

Concrete trigger: retain a valid capture or submit/complete a locally persisted resource, stop Halro as required, successfully change its Master Key/DEK, restart before the object's authorized retention/deletion boundary, then retrieve it or resume queued work. No malicious caller, corrupt ciphertext, expired TTL, or race is required. In-progress deferred work already deliberately fails on restart; that existing restart contract is **not** counted as SEC-01 impact.

## Existing defenses and contract adjudication

- **Offline lock, metadata copy-on-write, key checks, slot unwrap verification, audit/ledger key preservation:** protect metadata publication and accounting continuity. They do not traverse/decrypt retained files, so their success does not contradict SEC-01.
- **Recovery bridge:** `internal/app/vault_material.go:115` encrypts the *new* key under the old Vault. It supports interrupted publication, not recovering the old key from the new one. Finalizers clear it. A previous fingerprint is an identifier, not decryption material.
- **Plaintext upgrade cleanup:** `internal/app/provider_resources.go:182` skips objects with `vault.SealedEnvelope` headers; directory cleanup uses the same distinction. `internal/vault/vault.go:139` recognizes an envelope header without authenticating its ciphertext. An intact old-key sealed object survives this check; the upgrade cleanup is neither key migration nor a reason to discard these records during rotation.
- **Retention/deferred contract:** accepted ADR 0024, lines 108–136, describes explicit deletion, cool-off/TTL reclamation (default 24 hours), queued restart recovery, and no upstream twin. Rotation is not a documented early deletion boundary. Short retention bounds the population, not correctness within that window. The plaintext-upgrade exception does not authorize losing sealed objects on routine key changes.
- **Rotation contract:** `docs/runbooks/file-master-key-rotation.md:6` explicitly promises rewriting all Vault ciphertext. Its preconditions require stopping the server and a verified pre-rotation backup/old-key custody, but do not require empty retained stores. The KMS lifecycle runbook distinguishes same-DEK rewrap from new-DEK rotation and requires DEK rotation for suspected compromise; it is explicitly release-blocked and must not be quoted as production acceptance.
- **Backup/old-key custody is a real recovery defense:** `internal/app/backup.go:677`, `:696` includes both registered ObjectPath and InputObjectPath files in archives. Therefore a suitable pre-rotation archive with its original key can support restoring those resources; parent owns the actual drill. This backup source does not include the failure-capture directory. The reproduction's provider object is deliberately unregistered, so it is not itself evidence of official backup inclusion. Original ciphertext plus the old key remains decryptable in both reproductions. Do not call all affected data irreversibly destroyed or claim official backup restores capture payloads.
- **Post-rotation backup is not a migration:** backup file collection checks paths/file type and copies bytes; it does not reencrypt objects. A new-generation archive can therefore copy old-generation object ciphertext. This consequence is source-derived, not a completed restore drill.
- **Compromise nuance:** ciphertext remaining under an old DEK remains readable to someone possessing that DEK and ciphertext. This does not establish that anybody acquired either. Retaining historical backups under old keys is separately documented; the defect here also leaves current retained objects under that key.

## Synthetic verification and commands

All commands run from repository root, with `-count=1`; output redirected directly so exit codes are those of `go test`. Default sandbox succeeded; **no environment/cache/socket failure or escalation occurred in these adjudication runs**. Parent's earlier full-gate environment failures are separate evidence.

1. Inspected the original synthetic source, then reran exactly:

   ```sh
   go test -count=1 -v -overlay /private/tmp/halro-security-review-260905/overlay.json ./internal/app -run '^TestSecurityReviewRotationRetainedCiphertext$'
   ```

   **Exit 0**, log `original.log` in this adjudication's temporary directory. Real file rotation, real failure-capture store; synthetic on-disk content object. Before rotation capture read succeeds. After successful rotation: `capture found=false err=open failure capture: secret authentication failed`; unchanged resource ciphertext fails with the new key and opens with the old key. This is a passing defect-reproduction assertion, not a passing preservation invariant.

2. Independently adapted that single reproduction into `rotation_test.go`, overlaid as `internal/app/rotation_adjudication_test.go`. Used the repository's `kmsRotationFixture`, its fake-KMS factory, and `rotateKMSMasterKeyWithOptions` with deterministic fresh DEK and explicit operation ID. Reloaded the published primary slot through `unlockKMSMasterKey` and checked that it yields the expected new DEK. Resource scope is `resp_review:input`, matching the deferred-input scope. No provider request is sent.

   ```sh
   go test -count=1 -v -overlay /private/tmp/halro-security-rotation-adjudication-260905/overlay.json ./internal/app -run '^(TestAdjudicationKMSRetainedCiphertext|TestKMSRewrapPreservesMasterKeyCiphertextAndKeyVersion)$'
   ```

   Initial **exit 1** was a reviewer-written fixture compile error (`unlocked.Key` on a `[]byte`), corrected only in `/private/tmp`. Rerun **exit 0**, log `kms.log`. The fake-KMS DEK rotation reproduced the same capture and unchanged-object authentication failures; old key still opens the input object. The existing rewrap control passed, preserving fingerprint, generation, credential ciphertext, and key version and successfully unlocking the replacement primary slot.

| Invariant / counterhypothesis | Test evidence |
| --- | --- |
| Still-retained capture remains readable across completed file rotation | Violated by original reproduction. |
| Still-retained capture and resource input remain readable across completed DEK rotation | Violated by independent fake-KMS reproduction with persisted primary reload. |
| Rotation actually changes or migrates retained object bytes | Both reproductions assert bytes unchanged and new-key read fails. |
| Ciphertext was already corrupt / old key cannot read it | Refuted by old-key decrypt controls; capture also has successful pre-rotation read. |
| Same-DEK KEK rewrap necessarily causes this defect | Refuted by inspected derivation and passing existing rewrap control. |

## Coverage, gaps, and limitations

Read rotation and finalization paths for file and KMS; metadata rewrite/verification; Vault envelope/key derivation; bridge/key-slot lifecycle; runtime sealer wiring; failure capture open/read; provider-object write/read/cleanup/retention; deferred submission/resume/render; file-download fallback; backup object collection; relevant rotation runbooks and accepted deferred ADR. Reviewed the existing file/KMS fixture and rewrap tests and authorized SEC-01 reproduction. No unrelated reviewer reports were used.

Synthetic resource files are not registered public API resources. They prove the key/object mechanism; HTTP 404/503 and queued-state consequences above are source-traced, not independently executed endpoint journeys. No real AWS, production credentials, crash-point matrix, concurrency/race suite, all-object migration test, or binary restore drill was run. Parent retains ownership of runtime backup verification. Existing rotation tests cover metadata/credentials/MFA/audit/key slots but miss retained failure payloads and both resource roles; their passing names such as “AllMaterial” do not establish that broader invariant.

## Closure criteria (proposals, not implemented)

Define one explicit lifecycle for all retained key-dependent ciphertext: migrate it with recoverable publication semantics, or retain appropriately protected generation-specific decryption capability for its authorized lifetime. If temporarily unsupported, refuse rotation before mutation when affected live retained objects exist; do not silently declare successful migration. Cover file and fake-KMS DEK rotation with registered queued/completed resources and failure captures, old/new key assertions, retrieval and retention checks, interrupted recovery, and pre/post backup restore. Preserve the same-DEK rewrap distinction. Any deliberate early deletion policy requires an explicit product/retention contract rather than relying on the unrelated plaintext-upgrade cleanup.
