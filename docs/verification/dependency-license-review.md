# Dependency and License Review

Date: 2026-08-11

Halro is distributed under Apache-2.0. The source tree includes the project
license in `LICENSE`, required attribution in `NOTICE`, and the runtime
inventory in `THIRD_PARTY_NOTICES.md`. Release archives and the container carry
those files together with a versioned SPDX SBOM.

The root module currently declares 12 direct Go dependencies. Eleven are linked
into the `halro` runtime; `github.com/google/jsonschema-go` is used by tests and
release validation only. The embedded Admin UI has 11 direct runtime npm
dependencies. Versions below are the exact versions pinned by the reviewed
module and lock files.

## Go direct dependencies

| Module | Version | License | Distribution scope |
|---|---:|---|---|
| `github.com/aws/aws-sdk-go-v2` | 1.43.6 | Apache-2.0 | runtime |
| `github.com/aws/aws-sdk-go-v2/config` | 1.32.37 | Apache-2.0 | runtime |
| `github.com/aws/aws-sdk-go-v2/credentials` | 1.19.36 | Apache-2.0 | runtime |
| `github.com/aws/aws-sdk-go-v2/service/kms` | 1.55.6 | Apache-2.0 | runtime |
| `github.com/aws/smithy-go` | 1.27.8 | Apache-2.0 | runtime |
| `github.com/go-chi/chi/v5` | 5.3.1 | MIT | runtime |
| `github.com/google/jsonschema-go` | 0.4.3 | MIT | test/release tooling |
| `github.com/parquet-go/parquet-go` | 0.32.0 | Apache-2.0 | runtime |
| `go.etcd.io/bbolt` | 1.5.0 | MIT | runtime |
| `golang.org/x/crypto` | 0.55.0 | BSD-3-Clause | runtime |
| `golang.org/x/sys` | 0.47.0 | BSD-3-Clause | runtime |
| `gopkg.in/yaml.v3` | 3.0.1 | MIT and Apache-2.0 | runtime |

The 2026-08-21 refresh moved six direct versions and added, removed, and
relicensed nothing: the module path sets in `go.mod` and `go.sum` are identical
to the reviewed tree before it, so every row above is a version change rather
than an inventory change. Five are AWS SDK patch releases on the KMS custody
path described below. The sixth is `golang.org/x/crypto` 0.54.0 to 0.55.0, and
the runtime links exactly two packages from that module — `argon2` and
`blake2b` — both of which are byte-identical between the two versions, as is
the module LICENSE. Nothing that reaches the binary changed.

The AWS KMS custody path is part of this review. The linked AWS SDK config and
credential modules can resolve environment, shared-file, web-identity,
ECS/container, and EC2 IMDS workload credentials when Key Slot mode creates the
AWS adapter. File-mode startup is separately tested not to initialize the AWS
SDK or perform cloud calls. The SDK and smithy NOTICE text is carried in the
root `NOTICE`; all exact runtime modules, including transitive parquet helpers,
are listed in `THIRD_PARTY_NOTICES.md` and the release SBOM.

Resolved Go runtime modules were checked from the module cache. Licenses are
permissive MIT, BSD, or Apache-2.0; no GPL, AGPL, LGPL, MPL, SSPL, or BUSL module
is linked into the Go runtime.

## Admin UI direct runtime dependencies

| Package | Version | License |
|---|---:|---|
| `@hookform/resolvers` | 5.7.1 | MIT |
| `@tanstack/react-query` | 5.101.4 | MIT |
| `i18next` | 26.3.6 | MIT |
| `qrcode` | 1.5.4 | MIT |
| `react` | 19.2.8 | MIT |
| `react-dom` | 19.2.8 | MIT |
| `react-hook-form` | 7.85.0 | MIT |
| `react-i18next` | 17.0.11 | MIT |
| `uplot` | 1.6.32 | MIT |
| `zod` | 4.4.3 | MIT |

The Admin UI lockfile contains no CC-BY package. Its 12 MPL-2.0 entries are
`lightningcss` 1.33.0 plus eleven platform-specific optional binaries. They are
dev-only CSS build tooling and are not present in the generated Admin UI bundle
or final container. Source and build environments still retain their upstream
license metadata; this review does not relabel them as runtime dependencies.

The independent packages under `tests/compatibility/` install official SDK test
clients in CI. They are excluded from the embedded Admin UI inventory and are
not copied into release archives or the runtime container. Their lock files and
licenses remain part of the source/CI dependency surface and the corresponding
SDK jobs continue to audit them.

## Distribution requirements

- Preserve the project license plus dependency copyright/license notices in
  binary archives and container images.
- Carry the AWS SDK for Go and smithy-go upstream NOTICE text, plus Apache-2.0
  notices for Parquet and Apache-covered YAML portions.
- Generate the SPDX SBOM from the final clean release tree; this review is not
  a substitute for the artifact-specific SBOM.
- Keep `LICENSE`, `NOTICE`, and `THIRD_PARTY_NOTICES.md` in every binary archive
  and container image.
- Repeat this review whenever any dependency input below changes.

## Drift gate

CI runs `scripts/check-dependency-license-review.sh`. These are Git blob hashes
of the reviewed dependency inputs; a dependency change cannot pass until this
document is deliberately refreshed with the new inventory and hashes.

- `go.mod`: `5759531ea56103e64bbc3f981947d7592fa7973a`
- `go.sum`: `54bfd4b3cca98c81e2a0660047fe49e1f0cbe098`
- `web/package.json`: `5095111d843d2904d227a6150a5e798124cdd7c5`
- `web/package-lock.json`: `a5d111d431624d71d10e4e8cb75392382245c885`

The Go hashes moved for the six-module bump recorded above. The two web hashes
moved for `chore(release): v0.2.0` and again for `chore(release): v0.3.0`, each
of which bumped the `version` field in both files and changed nothing else — no
dependency was added, removed, or upgraded, so the inventory above still
describes the reviewed tree and the review date is unchanged. The gate hashes
whole files
rather than dependency sections, which is the right trade: it cannot be talked
out of noticing a change, at the cost of occasionally flagging one that carries
no dependency in it.

CI also runs `govulncheck`, npm audits, bundle scanning, repository notice
checks, and artifact SBOM generation. Those checks complement license review;
none individually replaces it.
