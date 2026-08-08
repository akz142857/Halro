# M11-03B AWS KMS Adapter evidence — 2026-08-03

Status: local implementation evidence complete; real AWS evidence pending.

This record covers Issue #59 and the implementation on
`codex/m11-aws-kms-adapter`. It must not be interpreted as completion of the
real-account gate until the final section is replaced with a successful,
sanitized smoke record.

## Implemented contract

- Official AWS SDK for Go v2 config and KMS modules at the versions pinned in
  `go.mod`; AWS SDK retry attempts are fixed at one.
- Workload Identity only: Web Identity/IRSA, ECS/EKS credential endpoint, or
  EC2 instance profile. Generic `AssumeRoleProvider` is rejected because its
  source credentials may be a static shared-file profile and the SDK result
  does not retain enough provenance to prove otherwise. Static
  environment/shared-file credentials, SSO and credential-process sources are
  rejected by the Adapter.
- AWS KMS symmetric Encrypt/Decrypt using a full customer-managed Key ARN and
  `SYMMETRIC_DEFAULT`.
- Exact region, account, Key ARN, HTTPS endpoint and algorithm allowlists are
  checked before the AWS client receives a request.
- Versioned five-field Encryption Context binds hashed instance and Slot IDs,
  protected-payload version and Primary/Recovery purpose without exposing raw
  identifiers to CloudTrail.
- Canonical 112-byte `HKMSKEY1` payload with strict length, magic, version,
  flags, reserved-field and instance/Slot binding checks.
- Stable error mapping, provider request IDs, bounded Retry-After, per-call
  timeout, total startup deadline, bounded attempts and full jitter.
- The Adapter has no transitive dependency on bbolt or Halro App, Audit,
  Backup, Domain, Gateway, Store or Vault packages. Gateway and provider-neutral
  KMS/Master Key packages have no AWS SDK dependency.

## Local verification

The following commands passed on Go 1.26.5:

```text
go test ./...
go test -race ./...
go vet ./...
GOTOOLCHAIN=go1.26.5 go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
go build -trimpath ./cmd/halro ./cmd/halro-deadman
sh -n tools/m11/aws-kms-smoke/run.sh
python3 -m unittest tools/m11/aws-kms-smoke/test_verify_cloudtrail.py
./tools/m11/check-kms-boundaries.sh
```

`govulncheck` reported zero reachable vulnerabilities. It reported one
vulnerability in an imported package and one in a required module whose
vulnerable symbols are not called. The same non-reachable baseline was already
recorded by M11-03A.

Negative coverage includes incorrect Key ARN/account/region/endpoint/algorithm,
static credentials, missing identity, throttling, permission, expired identity,
disabled/deleted key, timeout, invalid ciphertext/Context, malformed protected
payload, provider response substitution and oversized provider results.

CI validates the smoke shell, the CloudTrail correlation verifier, provider
boundaries, Test/Race/Vet, vulnerability scan and the standard single-artifact
build. Runtime request-path packages do not import the Adapter or AWS SDK.

## Real AWS gate — pending

The read-only environment check on 2026-08-03 found:

```text
credential source: shared-credentials-file
STS GetCallerIdentity: InvalidClientTokenId
HALRO_AWS_KMS_KEY_ARN: unset
```

No AWS resource was created or modified. Completion requires an existing
customer-managed symmetric KMS Key ARN and a valid approved Workload Identity
with Encrypt, Decrypt and CloudTrail lookup permissions. Run:

```text
HALRO_AWS_KMS_KEY_ARN='arn:aws:kms:REGION:ACCOUNT:key/UUID' \
  ./tools/m11/aws-kms-smoke/run.sh
```

The harness uses the existing Key only, correlates Encrypt/Decrypt request IDs
with CloudTrail, retains only hashed identifiers and booleans, and removes raw
temporary evidence. Archive its sanitized JSON here before changing this gate
to complete.
