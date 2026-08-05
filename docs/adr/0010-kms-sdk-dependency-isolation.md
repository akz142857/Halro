# ADR 0010: KMS SDK dependency and release isolation

Status: Accepted (2026-08-03)

Milestone tracking: `docs/milestones/milestone-m11-master-key-custody-aws-kms.md`

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

The reviewed production contract is implemented by `internal/kms` and freezes:

- exactly 112 plaintext bytes for the versioned `HKMSKEY1` protected payload;
- at most 64 KiB provider-neutral ciphertext, with each Adapter enforcing its
  smaller native limit (AWS Encrypt/Decrypt uses 6,144 bytes);
- 2,048-byte key reference, 128-byte algorithm and bounded opaque binding
  context fields;
- cloned mutable request/result buffers at ownership boundaries;
- stable classes `kms_transient`, `kms_throttled`,
  `kms_identity_not_ready`, `kms_permission_denied`,
  `kms_key_unavailable`, `kms_config_invalid`, `kms_ciphertext_invalid`,
  `kms_payload_invalid`, `kms_vault_mismatch` and
  `kms_adapter_unavailable`;
- automatic retry eligibility only for transient, throttled and
  identity-not-ready classes; all other classes fail fast at the current Slot;
- secret-safe public error text while retaining the native cause only for
  internal classification with `errors.Is`/`errors.As`.

`internal/kms/fakekms` implements this contract using authenticated encryption,
provider-binding tamper rejection, scripted typed faults, timeout and
cancellation. It is not selectable by production configuration.

## Decision

Heimdall accepts Option A: one module and one signed `heimdall` artifact. The
main binary and production container will include the official AWS SDK config
and KMS packages after the production Adapter lands. File mode selects and
constructs its backend before any AWS configuration, credential-chain or
client initialization, so linking the SDK does not make AWS a premise of the
core architecture or a runtime dependency of File mode.

The accepted evidence is:

- `docs/milestones/evidence/m11-03a-aws-sdk-spike-2026-08-03.md`;
- reproducible harness `tools/m11/aws-sdk-spike/run.sh`;
- provider-neutral contract and fake-KMS tests under `internal/kms`.

The spike pinned `config@v1.32.34` and `service/kms@v1.55.3`. Relative to the
cloud-neutral artifact it measured +15 modules, +3,132,416 binary bytes,
+3,145,728 production-container bytes and +15 SPDX packages, with zero new
reachable vulnerability, zero File-mode metadata requests and zero AWS
dependencies in the Gateway package graph. Clean test increased 0.27 seconds
and 25-run cold-start mean increased 2.8 milliseconds on the recorded arm64
host.

Publishing both artifacts would require 86.3% more combined binary bytes and
87.1% more combined container bytes per architecture, plus duplicate build,
SBOM, provenance, signature, patch and operator-selection paths. The modest
single-artifact increase is accepted to avoid that release and recovery risk.

### Resulting module and release layout

- one root Go module;
- one `heimdall` binary/container for File and AWS KMS modes;
- no `heimdall-aws` artifact, build tag, plugin or helper process;
- official AWS SDK config/KMS packages isolated below the AWS Adapter package;
- provider-neutral core and `internal/gateway` must not import AWS SDK types;
- File-mode tests must keep a live metadata/identity probe at zero requests;
- the existing single release pipeline remains responsible for test, Race,
  vulnerability scan, SBOM, provenance, signing and release labels;
- every release architecture repeats size/SBOM evidence at M11-07.

GCP and Azure do not inherit this conclusion and require a new measured ADR if
their implementation changes the release boundary.

## Evaluated options

M11 Phase 0 compared only the two release models relevant to the current AWS
production requirement:

### Option A: one Heimdall artifact — accepted

The main `heimdall` binary includes the AWS SDK and AWS KMS Adapter. File mode
does not initialize or call them unless AWS KMS is explicitly configured.

This option favors one installation and release path, at the cost of carrying
the AWS dependency and SBOM surface for File-only operators.

### Option B: core and AWS artifacts — rejected for M11

The project publishes `heimdall` without the AWS SDK and `heimdall-aws` with
the AWS SDK and Adapter. Both artifacts use the same source commit, core
packages, configuration model, CLI semantics, tests, release version, signing
process, and compatibility contract.

This option preserves a smaller cloud-neutral artifact, but its duplicate CI,
signing, distribution, documentation and vulnerability-management paths cost
more than the measured dependency reduction justifies. It is rejected for M11.

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

The evidence items above are satisfied by the accepted spike report and
contract tests. Production AWS SDK implementation may now proceed under this
ADR, but cannot bypass its package, runtime or release constraints.

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

## Consequences

- AWS dependency updates affect the single module, SBOM and release artifact;
  Dependabot/security review must treat config, KMS and their credential-chain
  dependencies as production scope.
- File-only operators carry approximately 3.1 MB and 15 SPDX packages that are
  not exercised in their mode; the zero-initialization/no-network contract is
  therefore a permanent regression gate.
- Release engineering keeps one artifact, signature, provenance statement and
  patch path rather than a provider-specific artifact matrix.
- AWS remains the only cloud implementation admitted by M11.
- No plugin framework is created by the accepted in-process Adapter layout.
- Provider-neutral core, Key Slot state, Vault validation and Gateway request
  processing remain free of AWS SDK types and calls.

## Implementation gate

Production AWS SDK implementation may merge only while all of the following
remain true:

1. the provider-neutral Master Key boundary and final `storage.master_key`
   configuration pass File-mode tests;
2. the reviewed `internal/kms` wrapper contract and typed error taxonomy pass;
3. fake-KMS contract, timeout, cancellation, and fault-injection tests pass;
4. the Option A/Option B spike evidence remains reproducible and archived;
5. this ADR remains `Accepted` for the one-artifact release model;
6. release CI builds, tests, scans, signs, proves and labels the single
   selected artifact.
