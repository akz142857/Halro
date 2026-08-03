# M11-04 dual-Slot initialization evidence — 2026-08-03

Status: local implementation evidence complete; real AWS dual-Slot gate pending.

This record covers Issue #60 on `codex/m11-kms-dual-slot-init`. The branch is
stacked on M11-03B and carries the pure implementation commit from M11-02. It
does not merge any open PR and does not implement File migration, rewrap or DEK
rotation.

## Implemented protocol

1. Static configuration and an initialization publication lock are checked
   before any KMS or persistent-state operation. Empty Key-Slot instances
   require explicit offline `heimdall init`; Runtime never auto-initializes one.
2. A 32-byte Master Key and instance ID are generated in memory.
3. Vault Key Check, Keyring and protected Audit material are created in memory.
4. Primary and Recovery use different allowlisted KMS Key ARNs. Each receives a
   Slot-specific `HKMSKEY1` payload and Encryption Context.
5. Each pending Slot is independently unwrapped, checked against its payload
   binding and fingerprint, and authenticated by the same Vault Key Check.
6. Only a production-ready descriptor containing active verified Primary and
   Recovery Slots is accepted by the initialization transaction.
7. Descriptor, Keyring, Vault Key Check, Audit HMAC envelope and Audit
   checkpoint are committed in one bbolt transaction in a staging data tree.
8. Ledger and four secret-safe Slot transition Audit records are durable before
   publication. The persisted Primary is independently unlocked and checked
   against the staged Vault.
9. The complete data directory is atomically renamed into place and its parent
   is synced while a sibling publication lock excludes Runtime writers.
10. All Master Key and payload buffers are cleared; no Master Key file is
    created.

Normal Runtime, Bootstrap, Admin and backup paths select only the configured
Primary Slot. Recovery is available only through the explicit offline command:

```text
heimdall key recover --config CONFIG --confirm-recovery-slot SLOT_ID
```

The confirmation must exactly match trusted configuration. Success appends
`security.master_key.recovery_used` with `break_glass_recovery` to the protected
Audit chain before returning. The CLI tells the operator to revoke temporary
AWS Recovery authorization immediately.

## Local verification

The following passed on Go 1.26.5:

```text
go test ./...
go test -race ./...
go vet ./...
GOTOOLCHAIN=go1.26.5 go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
go build -trimpath ./cmd/heimdall ./cmd/heimdall-deadman
```

Focused evidence includes:

- ten bbolt transaction kill points, with every root-state record absent after
  rollback;
- five staging/publication failure points plus a Recovery permission failure,
  with no live or retained staging data directory;
- two independent fake KMS cryptographic roots in different configured
  regions/accounts unlocking the same Vault;
- cross-Key ciphertext, instance Context and trusted-allowlist substitution
  rejection before partial startup;
- presence-only metadata never being considered initialized and static checks
  producing zero KMS calls;
- CLI init/recovery routing, Bootstrap, Admin and Runtime integration;
- zero KMS calls after Runtime construction during Gateway authentication and
  route lookup;
- raw filesystem, bbolt snapshot and decrypted backup inspection with no
  plaintext Master Key;
- a KMS-specific Secret Canary over logs, stable errors, Audit, Metrics,
  filesystem/bbolt and heap profile; it also rejects Key ARNs and KMS
  ciphertext on public/telemetry surfaces;
- an audited explicit Recovery verification path;
- zero reachable vulnerabilities from `govulncheck`.

## Real AWS gate — pending

The opt-in gate is:

```text
HEIMDALL_AWS_KMS_PRIMARY_KEY_ARN='arn:aws:kms:REGION:ACCOUNT:key/UUID' \
HEIMDALL_AWS_KMS_RECOVERY_KEY_ARN='arn:aws:kms:REGION:ACCOUNT:key/UUID' \
  ./tools/m11/aws-kms-dual-slot-smoke/run.sh
```

It uses existing Keys only, creates a temporary local Heimdall instance,
independently unlocks both Slots, executes the audited Recovery path and emits
only hashes/booleans. The current machine cannot run it: STS reports
`InvalidClientTokenId`, the credential source is a shared static test file, and
neither Key ARN is set. No AWS resource was created or modified.

Before this gate becomes complete, evidence must show two distinct Keys and at
least one reviewed independent failure-domain dimension. Runtime permission
must exclude Recovery Decrypt; the temporary initialization/recovery grant must
be revoked after the exercise. M11-03B's CloudTrail correlation gate must also
be complete.
