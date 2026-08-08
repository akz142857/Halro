# M11-03A AWS SDK module/artifact spike evidence

Date: 2026-08-03

Source baseline: `512d517af02c9ecad0385d8a8a469b0e181549b8`

Decision input for: `docs/adr/0010-kms-sdk-dependency-isolation.md`

## Experiment contract

The reproducible harness is `tools/m11/aws-sdk-spike/run.sh`. It creates two
temporary copies of the same source tree and does not change the repository's
production `go.mod`, `go.sum`, or source:

- core: existing `halro` artifact;
- AWS-linked: the same artifact plus official AWS SDK config and KMS packages;
- Option A publishes only the AWS-linked artifact as `halro`;
- Option B publishes core as `halro` and AWS-linked as `halro-aws`.

Reproduce with:

```sh
./tools/m11/aws-sdk-spike/run.sh /absolute/result/directory
```

The overlay calls `config.LoadDefaultConfig` and `kms.NewFromConfig` behind an
explicit experiment-only switch so those paths remain link-reachable for size
measurement. The production Adapter is intentionally absent from this spike.

## Environment and pinned inputs

| Input | Value |
|---|---|
| Host | macOS Darwin 25.6.0, arm64 |
| Go | 1.26.5, darwin/arm64 |
| Docker engine | 29.4.0, linux/arm64 |
| AWS config module | `github.com/aws/aws-sdk-go-v2/config@v1.32.34` |
| AWS KMS module | `github.com/aws/aws-sdk-go-v2/service/kms@v1.55.3` |
| Syft | repository CI digest `sha256:bd5357...9060e` |
| govulncheck | v1.6.0 with project toolchain Go 1.26.5 |

The official SDK documentation confirms that `config.LoadDefaultConfig` owns
the supported default credential chain, including web identity, ECS task role
and EC2 instance role, and that the SDK owns credential caching/rotation and
SigV4. Halro therefore does not implement or persist static AWS credentials.

References:

- <https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html>
- <https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/config>
- <https://docs.aws.amazon.com/kms/latest/APIReference/API_Encrypt.html>

## Results

| Measurement | Core | AWS-linked | Delta |
|---|---:|---:|---:|
| Go modules | 42 | 57 | +15 (+35.7%) |
| stripped binary | 19,765,074 B | 22,897,490 B | +3,132,416 B (+15.8%) |
| production container | 21,238,576 B | 24,384,304 B | +3,145,728 B (+14.8%) |
| SPDX packages | 21 | 36 | +15 (+71.4%) |
| isolated-cache clean build | 5.99 s | 6.22 s | +0.23 s (+3.8%) |
| isolated-cache full test | 41.53 s | 41.80 s | +0.27 s (+0.7%) |
| 25-run cold-start mean | 0.0328 s | 0.0356 s | +0.0028 s |
| reachable vulnerabilities | 0 | 0 | no change |
| File-mode metadata requests | — | 0 | pass |
| AWS deps in Gateway graph | — | 0 | pass |

Both vulnerability scans reported zero reachable vulnerabilities. Both also
reported the same one imported-package and one required-module vulnerability
whose vulnerable symbols are not called; adding AWS did not change that result.

The 15 new Go/SPDX packages are the AWS SDK root, config, credentials, IMDS,
KMS, STS, SSO/OIDC/signin, Smithy and their internal endpoint/signing support.
The `go.mod` delta contains two direct and thirteen indirect requirements; the
`go.sum` delta contains 56 lines.

Raw artifact digests from the accepted run:

| Artifact | SHA-256 |
|---|---|
| `summary.env` | `b5aee80b6f38b0fb8622d849165af2466edc39df96bce002c8cd0ae31b9315e1` |
| `go.mod.diff` | `5c49fa11f622c0c41e949e6025b56a349e7958ceed3020a2abc68567ae423744` |
| `go.sum.diff` | `6f64a011a85c566aa782a4b0d3f46b607af6569c87090592e83604bde975aee6` |
| core SPDX JSON | `d0d80e0729fee9554c3352def5f68fa429c92779a5a6451993df83695f1aac1f` |
| AWS SPDX JSON | `ca30d01c52ec96d9cde483eae1d7a88e61e7d2be46a4edcf20865114bc9b9abc` |
| each govulncheck report | `3ec96e53373d0ec36171fd50800a5ad85a44c607437f8f900371203b70f314d3` |

## File-mode and hot-path proof

The AWS-linked binary ran both `version` and static `config check` with:

- an enabled, live local EC2 metadata endpoint probe;
- nonexistent shared config and credential paths;
- no experiment activation switch.

The probe received zero requests. The harness fails if the log is nonempty.
It also evaluates `go list -deps ./internal/gateway` inside the AWS-linked tree
and fails if an AWS SDK or AWS KMS Adapter package enters that graph. KMS is a
cold-start/admin lifecycle dependency only; it is not a Gateway request-path
dependency.

## Workload Identity complexity

The selected implementation path is deliberately small:

1. core validates trusted region, endpoint and key allowlists before Adapter
   construction;
2. only AWS Key-Slot mode calls `config.LoadDefaultConfig` with the validated
   region and controlled SDK retry settings;
3. the official default chain supplies IRSA/web identity, ECS task role or EC2
   instance role and owns refresh, caching and signing;
4. `kms.NewFromConfig` constructs the narrow KMS client;
5. File mode never calls steps 2–4.

No access-key fields, credential files, SigV4 implementation or token-refresh
code are added to Halro. Real identity and KMS calls remain M11-03B evidence.

## Release-cost comparison

| Concern | Option A: one artifact | Option B: core + AWS artifacts |
|---|---|---|
| Per-AWS-install bytes | 22.90 MB binary / 24.38 MB container | same AWS artifact size |
| Published bytes per architecture | 22.90 MB binary | 42.66 MB combined (+86.3%) |
| Container bytes per architecture | 24.38 MB | 45.62 MB combined (+87.1%) |
| CI/build matrices | one | two |
| SBOM/provenance/signatures | one set | two sets |
| Security patch publication | one coordinated release | two artifacts must remain identical and patched |
| Operator failure mode | no artifact selection | wrong artifact can produce Adapter-unavailable failure |
| File-only dependency surface | includes 15 AWS packages | retains smaller core surface |

The AWS-linked increase is approximately 3.1 MB and adds no reachable finding,
while two artifacts almost double published bytes and double the release,
signature, provenance and patch-coordination paths. Because AWS is a real M11
production extension and the runtime/no-call boundary is proven independently,
the release-risk reduction outweighs the modest File-only artifact increase.

## Decision recommendation

Accept Option A:

- one Go module;
- one `halro` binary and production container;
- the official AWS config and KMS modules are direct dependencies after
  M11-03B adds the production Adapter;
- File mode selects its backend before any AWS configuration or identity load;
- AWS packages remain outside provider-neutral core and Gateway hot paths;
- the existing single-artifact CI, SBOM, vulnerability scan, provenance and
  signing pipeline remains authoritative.

GCP/Azure packaging is not inferred from this decision. A future provider must
re-measure its own dependency/release impact.

## Limits and follow-up gates

This run measures the production linux/arm64 container and darwin/arm64 binary.
M11-07 release evidence must repeat artifact-size/SBOM checks for every release
architecture. M11-03B must provide real AWS Workload Identity, Encrypt/Decrypt,
error mapping and CloudTrail evidence; this spike deliberately makes no real
AWS request.
