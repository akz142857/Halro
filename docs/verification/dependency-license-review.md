# Dependency and License Review

Date: 2026-09-11

Halro is distributed under Apache-2.0. The source tree includes the project
license in `LICENSE`, required attribution in `NOTICE`, and the runtime
inventory in `THIRD_PARTY_NOTICES.md`. Release archives and the container carry
those files together with a versioned SPDX SBOM.

The root module currently declares 12 direct Go dependencies. Eleven are linked
into the `halro` runtime; `github.com/google/jsonschema-go` is used by tests and
release validation only. The embedded Admin UI has 10 direct runtime npm
dependencies. Versions below are the exact versions pinned by the reviewed
module and lock files.

## Go direct dependencies

| Module | Version | License | Distribution scope |
|---|---:|---|---|
| `github.com/aws/aws-sdk-go-v2` | 1.46.0 | Apache-2.0 | runtime |
| `github.com/aws/aws-sdk-go-v2/config` | 1.33.3 | Apache-2.0 | runtime |
| `github.com/aws/aws-sdk-go-v2/credentials` | 1.20.3 | Apache-2.0 | runtime |
| `github.com/aws/aws-sdk-go-v2/service/kms` | 1.59.0 | Apache-2.0 | runtime |
| `github.com/aws/smithy-go` | 1.28.1 | Apache-2.0 | runtime |
| `github.com/go-chi/chi/v5` | 5.3.2 | MIT | runtime |
| `github.com/google/jsonschema-go` | 0.4.3 | MIT | test/release tooling |
| `github.com/parquet-go/parquet-go` | 0.32.0 | Apache-2.0 | runtime |
| `go.etcd.io/bbolt` | 1.5.0 | MIT | runtime |
| `golang.org/x/crypto` | 0.56.0 | BSD-3-Clause | runtime |
| `golang.org/x/sys` | 0.48.0 | BSD-3-Clause | runtime |
| `gopkg.in/yaml.v3` | 3.0.1 | MIT and Apache-2.0 | runtime |

The 2026-08-28 refresh moved six direct versions and added, removed, and
relicensed nothing: the module path sets in `go.mod` and `go.sum` are identical
to the reviewed tree before it, so every row above is a version change rather
than an inventory change, and every one of the six module LICENSE files is
byte-identical across its bump. Five are AWS SDK and smithy patch releases on
the KMS custody path described below. The sixth is `github.com/go-chi/chi/v5`
5.3.1 to 5.3.2, which is the one that moves behaviour and not only a version:
it de-duplicates the method list chi records behind a 405, and it changes what
`Routes()` and `Walk()` report for a `Mount()` stub handler. The runtime links
only the root `chi` package and not `chi/middleware`, where the rest of that
release landed, and the frozen Admin route contract — which enumerates the
router through `chi.Walk` and compares it against an exact expected set — still
matches.

The 2026-09-05 AWS refresh then moved the five AWS SDK and smithy rows above,
plus ten version-pinned AWS transitive modules, and again added, removed, and
relicensed nothing. The old and new LICENSE files are byte-identical for all 15
changed modules; the root AWS SDK and smithy NOTICE files are also
byte-identical. The shared transport changes move content-length calculation
into `SetStream`, add an opt-in connection read timeout, and fix the logger
middleware insertion point. The KMS client also moves credential-source user
agent attribution from per-request middleware to client construction. Halro
does not set `AWS_ENABLE_DEFAULT_SOCKET_TIMEOUT_2026`; its KMS Encrypt/Decrypt
surface, encryption context, retry policy, error classification, and file-mode
no-cloud-call boundary remain covered by the KMS and application tests.

The 2026-09-11 Go refresh moved four direct AWS SDK modules, nine
version-pinned AWS transitive modules, `golang.org/x/crypto`, and
`golang.org/x/sys`. It added, removed, and relicensed nothing: the module path
sets in `go.mod` and `go.sum` are unchanged. The AWS SDK modules remain
Apache-2.0, while the two Go subrepositories remain BSD-3-Clause. The KMS
custody path, credential discovery boundary, cryptographic helpers, and
platform syscall surface remain covered by the full Go and compatibility test
suites.

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
| `@hookform/resolvers` | 5.9.1 | MIT |
| `@tanstack/react-query` | 5.102.8 | MIT |
| `i18next` | 26.4.2 | MIT |
| `qrcode` | 1.5.4 | MIT |
| `react` | 19.2.8 | MIT |
| `react-dom` | 19.2.8 | MIT |
| `react-hook-form` | 7.87.0 | MIT |
| `react-i18next` | 17.0.13 | MIT |
| `uplot` | 1.6.32 | MIT |
| `zod` | 4.5.4 | MIT |

The same 2026-08-28 refresh moved five of the rows above — `@hookform/resolvers`
5.7.1 to 5.9.1, `@tanstack/react-query` 5.101.4 to 5.102.3, `i18next` 26.3.6 to
26.4.0, `react-hook-form` 7.85.0 to 7.86.0, `react-i18next` 17.0.11 to 17.0.12 —
and every one stayed MIT. It also moved four dev dependencies that are absent
from the table because they are not shipped: `@types/react-dom`,
`@vitejs/plugin-react`, `vite` 8.2.1 to 8.2.2 and `vitest`. The
`@hookform/resolvers` minor releases adopt Joi 18 and Vest 6 in resolvers this
project does not import, and neither package is in the lockfile, so no entry was
added to the dependency surface. The build-tooling bump did rewrite
`internal/webui/dist` — chunk contents and hashed names moved, and one
auto-named shared chunk regrouped — with no change to which packages reach the
bundle.

The 2026-09-05 Admin UI refresh then moved five runtime dependencies:
`@tanstack/react-query` 5.102.3 to 5.102.8, `i18next` 26.4.0 to 26.4.1,
`react-hook-form` 7.86.0 to 7.87.0, `react-i18next` 17.0.12 to 17.0.13, and
`zod` 4.4.3 to 4.5.4. It also moved the dev-only
`@testing-library/react` 16.3.2 to 16.3.3 and `@vitejs/plugin-react` 6.1.0 to
6.1.1. The lockfile package-node set is unchanged; the only transitive version
movement is `@tanstack/query-core` alongside `@tanstack/react-query`. All eight
changed package entries remain MIT, their installed packages carry MIT license
files, and none changed distribution scope. The embedded Admin UI bundle was
rebuilt from the reviewed lockfile so its content-hashed assets match the
source dependency tree.

The 2026-09-11 Admin UI refresh moved the runtime `i18next` package from
26.4.1 to 26.4.2 and the dev-only `@types/react-dom` from 19.2.5 to 19.2.7
and `vitest` from 4.1.11 to 5.0.0. All three remain MIT licensed. The Vitest
major update changes only test tooling; its lockfile adds the MIT-licensed
`@jridgewell/resolve-uri` and `@jridgewell/trace-mapping` packages and removes
four dev-only MIT packages. No runtime package was added, removed, or
relicensed, and the embedded Admin UI bundle was rebuilt from the updated
lockfile.

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

## Official SDK compatibility dependencies

The compatibility clients are now a first-class CI supply-chain surface. Their
Go module and npm package/lock files, plus the Python direct input and fully
hashed transitive lock, are included in the drift gate below. Both ordinary CI
and the release workflow run ecosystem-native vulnerability checks before the
black-box contracts: `govulncheck` for the nested Go module, `npm audit` for the
Node lock, and `pip-audit` for the complete hashed Python lock.

| Ecosystem | Reviewed direct dependencies | License |
|---|---|---|
| Go | `github.com/anthropics/anthropic-sdk-go` 1.71.0 | MIT |
| Go | `github.com/openai/openai-go/v3` 3.56.0 | Apache-2.0 |
| Node | `@anthropic-ai/sdk` 0.124.0 | MIT |
| Node | `openai` 7.10.0 | Apache-2.0 |
| Python | `anthropic` 1.4.0 | MIT |
| Python | `openai` 3.8.0 | Apache-2.0 |
| Python tooling | `pip-audit` 2.9.0 | Apache-2.0 |

The resolved Go compatibility graph is MIT, BSD-3-Clause, or Apache-2.0. The
Node lock contains MIT, Apache-2.0, and Unlicense packages. The 42-package
Python lock contains MIT, Apache-2.0, BSD-2-Clause, BSD-3-Clause, PSF/PSFL,
and one MPL-2.0 certificate bundle (`certifi`); all are test-only and none are
distributed in Halro artifacts. The `pip-audit` tool and its transitive packages
are deliberately in the same hash-checked lock, so the scanner is not fetched
through an unreviewed side channel during the job.

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

- `go.mod`: `89c91b3130972bc2b5c676e9e93266c1721fb9a8`
- `go.sum`: `b0ef94c14373b0cdb85a832fc1374dbdfe20820f`
- `web/package.json`: `f1eb5429cd8088ea8719c5c0a5a59074df5369d1`
- `web/package-lock.json`: `14664cea8cb80a1a23124b7f707a1a315d573da9`
- `tests/compatibility/go/go.mod`: `877e27308fd60916f0a462a88d301355fc66e084`
- `tests/compatibility/go/go.sum`: `930d790a4f664ad7a0ee641d22db7debb6c3ded5`
- `tests/compatibility/node/package.json`: `035be267a4afb3a23a447b7c95e963a82d9bd8db`
- `tests/compatibility/node/package-lock.json`: `c2498763cfbc484745d67b84f179a891c2ff42f7`
- `tests/compatibility/python/requirements.in`: `951765ed632ee780f88b1f49cd7f12ab3441a67e`
- `tests/compatibility/python/requirements.txt`: `f4a5887a32bcf876cafad3eb22b7fbe3bbf63dcf`

The Go hashes last moved for the 2026-09-11 Go refresh recorded
above. The two web hashes last moved for the 2026-09-11 Admin UI refresh
recorded above, before that for the 2026-09-05 seven-direct-package Admin UI
refresh, and before that for the nine-package Admin UI bump,
and before that only
for `chore(release): v0.2.0`, again for `v0.3.0`, again for `v0.4.0`, and again
for `v0.5.0`, `v0.6.0`, `v0.7.0`, and now `v0.7.1`, each of which bumped the
`version`
field in both files and changed nothing else.
Nothing in any of it added, removed, or relicensed a dependency, so the
inventory above still describes the reviewed tree. The gate hashes whole files
rather than dependency sections, which is the right trade: it cannot be talked
out of noticing a change, at the cost of occasionally flagging one that carries
no dependency in it.

CI also runs `govulncheck`, npm audits, bundle scanning, repository notice
checks, and artifact SBOM generation. Those checks complement license review;
none individually replaces it.
