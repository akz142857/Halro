# ADR 0010: KMS SDK dependency and release isolation

Status: Proposed — pending M11 AWS SDK spike (2026-08-03)

Milestone tracking: `docs/milestone-m11-master-key-custody-aws-kms.md`

## Context

Heimdall is intentionally self-contained and cloud-neutral. File mode must
remain a complete, first-class operating mode that neither configures nor calls
a cloud service. AWS KMS is the first optional production extension because it
matches the current deployment environment; it is not a premise of the core
architecture and does not commit the project to AWS.

AWS support requires the official AWS SDK for workload identity and KMS calls.
That SDK changes the dependency graph, binary or artifact size, SBOM,
vulnerability surface, build time, and security-update cadence. The production
benefit and release cost must therefore be measured before choosing how it is
packaged.

Build tags do not isolate a module graph: referenced modules remain in
`go.mod` and `go.sum`. Runtime helpers or dynamically loaded plugins would add
IPC authentication, another privileged process or executable boundary, version
skew, and an additional plaintext-key transfer. M11 does not need those
mechanisms.

## Fixed architectural constraints

The following decisions are independent of the packaging spike:

- Heimdall core owns Key Slot state, trusted configuration, protected-payload
  validation, Vault Key Check, retry policy, Audit, Metrics, and persistence;
- AWS support uses a narrow, explicit, in-process Adapter boundary;
- the Adapter implements only provider authentication and wrap/unwrap calls;
- no runtime plugin discovery, dynamic loading, plugin directory, hot reload,
  plugin marketplace, or out-of-process KMS helper is introduced;
- File mode remains usable without AWS credentials, network access, or AWS
  configuration;
- AWS SDK code must not appear in request hot paths;
- GCP and Azure implementations and their packaging are outside M11.

The core contract must not expose bbolt transactions, Vault objects, raw
Credential records, or request-path behavior. Its conceptual surface is:

```go
type KMSWrapper interface {
	Provider() string
	Wrap(ctx context.Context, request WrapRequest) (WrapResult, error)
	Unwrap(ctx context.Context, request UnwrapRequest) (UnwrapResult, error)
}
```

Requests contain validated provider-neutral inputs plus an opaque provider
binding context where required. Results contain only the protected ciphertext
or plaintext payload and stable metadata required for Audit. Adapters return
typed error classes; they do not own retry loops, fallback, Slot selection,
Vault validation, or persistence transactions.

## Decision pending

M11 Phase 0 will compare only the two release models that are relevant to the
current AWS production requirement:

### Option A: one Heimdall artifact

The main `heimdall` binary includes the AWS SDK and AWS KMS Adapter. File mode
does not initialize or call them unless AWS KMS is explicitly configured.

This option favors one installation and release path, at the cost of carrying
the AWS dependency and SBOM surface for File-only operators.

### Option B: core and AWS artifacts

The project publishes `heimdall` without the AWS SDK and `heimdall-aws` with
the AWS SDK and Adapter. Both artifacts use the same source commit, core
packages, configuration model, CLI semantics, tests, release version, signing
process, and compatibility contract.

This option preserves a smaller cloud-neutral artifact, at the cost of two CI,
signing, distribution, documentation, and vulnerability-management paths.

No default is selected in this ADR until the spike is complete. In particular,
this ADR does not pre-authorize separate GCP or Azure artifacts.

## Required spike evidence

The spike must record reproducible measurements for both options:

1. `go.mod` and `go.sum` changes;
2. binary and production container size changes;
3. clean build, test, and cold-start impact;
4. SBOM package count and vulnerability-report impact;
5. Workload Identity integration complexity;
6. one-artifact versus two-artifact CI, signing, release, and patch cost;
7. proof that File mode performs no AWS initialization or network calls;
8. proof that KMS remains outside the Gateway request hot path.

After review, this ADR will be updated to `Accepted`, name the selected option,
record the evidence location, and state the resulting module and release
layout. AWS SDK implementation may not be merged before that update.

## Rejected alternatives

### Add all planned cloud SDKs now

Rejected. Only AWS is required by M11. Adding speculative GCP and Azure
dependencies or artifacts would create cost without a validated implementation
or production requirement.

### Use build tags as dependency isolation

Rejected because build tags remove compiled code but do not by themselves
isolate `go.mod`, `go.sum`, SBOM review, or dependency-update pressure.

### Invoke cloud CLIs

Rejected because CLI output and authentication behavior are not a stable
application protocol, error classification is lossy, and secret-bearing
subprocess handling is difficult to secure and test.

### Use runtime helper processes or a general plugin system

Rejected for M11 because they introduce IPC authentication, deployment and
version skew, another crash boundary, and another plaintext-key memory copy.
A future need for independently deployed third-party providers would require a
new ADR; it must not be inferred from the current in-process Adapter boundary.

### Reimplement AWS authentication with only the standard library

Rejected because reproducing workload identity, SigV4 signing, token refresh,
endpoint selection, and retry semantics creates a security-sensitive parallel
SDK with greater maintenance risk than the official client.

## Consequences before the final decision

- The core/Adapter ownership boundary can be implemented and contract-tested
  without choosing an artifact layout.
- Release engineering cannot assume either a single artifact or a permanent
  family of provider-specific artifacts yet.
- AWS remains the only cloud implementation admitted by M11.
- No plugin framework is created, regardless of which packaging option wins.
- The selected option must preserve identical security behavior and operator
  semantics for AWS KMS; packaging must not fork the product model.

## Implementation gate

No production AWS SDK implementation may merge before all of the following are
present:

1. the provider-neutral Master Key boundary and final `storage.master_key`
   configuration pass File-mode tests;
2. the wrapper contract and typed error taxonomy are reviewed;
3. fake-KMS contract, timeout, cancellation, and fault-injection tests pass;
4. the Option A/Option B spike evidence is archived and reviewed;
5. this ADR is updated to `Accepted` with the selected release model;
6. release CI can build, test, scan, sign, and label the selected artifact set.
