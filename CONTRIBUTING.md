# Contributing to Heimdall

Thank you for helping improve Heimdall. Security, accounting correctness, and
backward-compatible API behavior take priority over feature count.

## Before opening work

- Search existing issues and pull requests first.
- Use a GitHub Discussion for broad design exploration when Discussions are
  enabled; otherwise open a focused feature request.
- For a security vulnerability, follow [SECURITY.md](SECURITY.md) instead of
  opening a public issue.
- For a large or compatibility-breaking change, agree on the contract in an
  issue before implementation.

## Development setup

Requirements:

- Go 1.26.5 or later;
- Node.js 22 and npm for the Admin console;
- Docker only for container validation.

```bash
git clone https://github.com/akz142857/Heimdall.git
cd Heimdall
make build
make test
make vet
```

Run the complete local gate before requesting review:

```bash
go test ./...
go test -race ./...
go vet ./...
cd web
npm ci --ignore-scripts
npm run typecheck
npm test
npm run build
cd ..
git diff --exit-code -- internal/webui/dist
```

Do not enable real Provider smoke tests in ordinary CI. They are billable and
require isolated, budget-limited credentials.

## Change expectations

- Keep the single-binary, zero-external-service operating model.
- Preserve fail-closed accounting, secret handling, SSRF defenses, and bounded
  resource use.
- Add tests for behavior changes and regression tests for bug fixes.
- Update public contracts and user/operator documentation when behavior changes.
- Never log or persist Provider keys, Gateway keys, prompts, response bodies,
  raw source IPs, or authorization headers in tests or fixtures.
- Do not commit local data, Master Keys, `.env` files, generated profiles,
  backups, Provider evidence, or private planning documents.
- Keep generated `internal/webui/dist` synchronized with `web/src`.

## Go style

- Run `gofmt` on changed Go files.
- Prefer standard-library primitives and small interfaces.
- Wrap errors with operational context without exposing sensitive values.
- Avoid high-cardinality Prometheus labels.
- Keep durable event schemas backward compatible or provide a tested migration.

## Frontend style

- Use the typed Admin API client and TanStack Query cache conventions.
- Keep secrets and CSRF material in memory only; never use browser persistence.
- Maintain keyboard operation, visible labels, focus management, and the bundle
  size/artifact gates.
- Rebuild the embedded production bundle with `npm run build`.

## Commits and pull requests

- Use a short imperative subject; Conventional Commit prefixes are encouraged
  (`feat:`, `fix:`, `docs:`, `test:`, `chore:`).
- Keep a pull request focused and explain risk, compatibility, security impact,
  and validation evidence.
- Link the relevant issue when one exists.
- Confirm that no secrets or private data are present in the diff.
- Maintainers may request smaller commits, additional fault-injection tests, or
  an ADR for architecture changes.

By contributing, you agree to follow [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)
and to license your contribution under the repository's project license.
