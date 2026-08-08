# Dependency and License Review

Date: 2026-07-31

Halro is distributed under Apache-2.0. The source tree includes the exact
project license in `LICENSE`, required attribution in `NOTICE`, and the runtime
dependency inventory in `THIRD_PARTY_NOTICES.md`. Release archives and the
container image carry these files together with a versioned SPDX SBOM.

The single binary has five direct Go dependencies. The embedded admin UI has seven runtime npm dependencies; build/test dependencies are not shipped as separately executable services.

## Go runtime dependencies

| Module | Version | License |
|---|---:|---|
| `github.com/go-chi/chi/v5` | 5.2.2 | MIT |
| `github.com/parquet-go/parquet-go` | 0.25.1 | Apache-2.0 |
| `go.etcd.io/bbolt` | 1.4.0 | MIT |
| `golang.org/x/crypto` | 0.32.0 | BSD-3-Clause |
| `gopkg.in/yaml.v3` | 3.0.1 | MIT and Apache-2.0 |

Transitive Go dependencies use permissive MIT, BSD, or Apache-2.0 terms. Their license files were inspected from the resolved module cache. No GPL/AGPL/copyleft runtime dependency was found.

## Admin UI runtime dependencies

React, React DOM, React Hook Form, Zod, TanStack Query/Table, the hook-form resolver, uPlot, and the locally bundled `qrcode` enrollment renderer declare MIT licenses. The lockfile also contains permissive MIT, BSD, ISC, Apache-2.0, BlueOak, CC0, and CC-BY metadata for build/test/browser-data packages. No GPL/AGPL runtime package was found.

## Distribution requirements

- Preserve the project license plus dependency copyright/license notices in binary archives and container images.
- Include Apache-2.0 notices for Parquet and the Apache-covered portions of YAML.
- Preserve attribution for generated browser compatibility data where applicable to source distributions.
- Generate an SBOM from the final, clean release tree; this review is not a substitute for the release SBOM.
- Repeat the inventory whenever `go.mod`, `go.sum`, `package.json`, or `package-lock.json` changes.
- Keep `LICENSE`, `NOTICE`, and `THIRD_PARTY_NOTICES.md` in every binary archive
  and container image; the release and repository-hygiene gates enforce this.

## Automated checks

CI runs `govulncheck` and `npm audit`. License compatibility is reviewed from pinned module/package metadata; final archives still require an SBOM and bundled notice verification.
