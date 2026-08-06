# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Heimdall: single-binary, security-first LLM gateway (Go). One governed API for multiple
model providers (OpenAI, Anthropic, Azure OpenAI, DeepSeek, Gemini Beta, AWS Bedrock
Beta, AWS Bedrock Mantle Beta), with credentials, budgets, routing, redaction, audit, and
usage accounting all owned locally — no external DB, cache, CDN, or browser-side secret
storage. Ships with an embedded React Admin console (`web/`) built into
`internal/webui/dist` and compiled into the Go binary.

Security, accounting correctness, and backward-compatible API behavior take priority
over feature count.

## Commands

Build/run:
```bash
make build            # bin/heimdall + bin/heimdall-deadman
make start             # build + start using CONFIG=config.yaml (creates config/storage on first run)
make dev               # build frontend, then `go run ./cmd/heimdall start`
make frontend          # npm ci + npm run build for web/, output embedded at internal/webui/dist
```

Test/lint (run before considering any change done):
```bash
go test ./...
go test -race ./...          # required for concurrency/lifecycle changes
go vet ./...
cd web && npm ci --ignore-scripts && npm run typecheck && npm test -- --run && npm run build && cd ..
git diff --exit-code -- internal/webui/dist   # fails if the embedded bundle is stale after web/ changes
```
Or run the full local gate: `make check` (test, race, vet, frontend-test, observability-check).

Single test:
```bash
go test ./internal/gatewayapi/ -run TestName -v
cd web && npx vitest run <path/to/file.test.ts>
```

Other:
```bash
make backup            # offline encrypted backup (requires Heimdall stopped); see docs/guides/backup-restore.md
make observability-check   # validates deploy/observability/ Prometheus/Alertmanager config
make reset CONFIRM=RESET   # destroys data dir + master key, then reinits — destructive, confirm before ever running
```

Real-Provider smoke tests are opt-in, billable, and never enabled in ordinary CI/dev
runs (see `docs/verification/provider-real-matrix.md`).

## Architecture

Request flow (see README's diagram, this is the mental model to keep while editing):

```
Client / SDK  →  Protocol Facade (openaiapi / anthropicapi / gatewayapi)
              →  Auth / policy / budget / redaction / accounting (gateway, auth, budget, tokenguard, redaction)
              →  Capability-aware router → versioned Provider Profile (provider/{openai,anthropic,bedrock,bedrockmantle,gemini})
              →  SafeTransport → upstream provider HTTP
```

Key `internal/` packages and what owns what:
- `gatewayapi`, `openaiapi`, `anthropicapi` — protocol facades: OpenAI-compatible
  Chat/Embeddings/Responses, Anthropic-compatible Messages. Each translates its wire
  protocol into a semantic operation before anything provider-specific happens.
- `semantic` — the provider-agnostic operation model everything routes through.
- `provider/*` — one package per upstream, each bound to an immutable, versioned
  Provider Profile that fixes its Access Surface, credential scheme, and capability
  evidence. Capability filtering happens before Provider I/O; unsupported fields are
  rejected, never silently dropped.
- `safetransport` — the only path to the network for provider/webhook calls: HTTPS-only,
  explicit host allowlists, DNS/IP validation, pinned dialing, no redirects, no env
  proxies. Never bypass this for outbound calls.
- `auth`, `adminauth` — Gateway Key auth (SHA-256-stored, `gw_...` keys) and Admin session
  auth (TOTP 2FA, CSRF, revision-checked mutations).
  Provider credentials are AES-256-GCM encrypted with audience-bound AAD (`kms`, `vault`,
  `masterkey`).
- `budget`, `limiter`, `tokenguard` — Project-level enforcement (RPM/TPM/concurrency,
  budget reservations, CIDR, Token Guard policy) applied before any unsafe upstream work.
  A budget reservation must be durable *before* a Provider request is made.
- `ledger` — the accounting authority, a framed/checksummed WAL. `usage` (Parquet
  partitions) and bbolt-backed aggregates in `store`/`domain` are rebuildable derivatives
  and must never be treated as sources of truth for balances. Attempt settlement
  atomically releases the reservation and commits cost/tokens; ambiguous Provider
  outcomes are conservatively accounted, never silently refunded.
- `audit` — append-only, integrity-checked audit trail (see `docs/contracts/audit-integrity.md`).
- `store`, `domain` — bbolt-backed transactional metadata (Projects, Routes, Deployments,
  Credentials, pricing, admin). Authoritative mutations validate before commit and
  preserve revision/tombstone/rollback/audit invariants.
- `redaction`, `contentscan` — must stay correct across recursive JSON, tool args, and
  chunked streaming boundaries; never buffer an unbounded stream to do it.
- `sse` — bounded SSE parsing/writing shared by streaming endpoints.
- `circuit`, `idempotency`, `compatibility` — retry/fallback bounding, idempotency keys,
  and the machine-readable endpoint compatibility manifests
  (`docs/compatibility/endpoint-manifests.json`).
- `app` — composition root wiring Admin HTTP handlers (`admin_*.go`) to the packages above.
- `webui` — embeds the built `web/` bundle (`internal/webui/dist`) into the Go binary.
- `deadman` (+ `cmd/heimdall-deadman`) — an independently deployed watchdog: checks
  Heimdall/Prometheus/Alertmanager readiness and sample freshness, sends heartbeat and
  down/up events to a separate receiver. Deliberately outside Heimdall's own failure domain.
- `backup` — offline encrypted backup/restore; `.hmbk` archives plus a dedicated backup
  key, independent from `master.key`.

Admin console path a human follows: `Credential → Provider → Deployment → Route →
Project → Gateway Key`. Applications only ever see the Gateway Key + a public model
alias; they never see the Provider credential or upstream model identifier.

Frontend (`web/`): React 19 + TanStack Query for the Admin API client, `web/src/design-system`
for shared UI, `web/src/i18n` for zh-CN/en, `web/src/pages` for screens. CSRF material
and secrets are kept in memory only — never persisted to browser storage (the production
bundle is scanned for secret canaries). Rebuild with `npm run build` and commit the
resulting `internal/webui/dist` whenever `web/src` changes — CI fails on drift.

## Invariants to preserve (violations are the highest-severity class of bug here)

- **Single-writer, single data directory.** v1 consistency boundary is one process
  owning one data dir exclusively (locked). Never introduce shared-directory or
  multi-writer behavior implicitly. Docker/Kubernetes must run exactly one replica
  (`Recreate` strategy, not rolling).
- **Ledger WAL is the accounting authority.** bbolt checkpoints, in-memory aggregates,
  and Parquet usage files are derivatives; don't let them diverge into a second source
  of truth.
- **Fail-closed, not fail-open**, for corrupt/unavailable/ambiguous/stale state —
  everywhere: auth, budget, redaction, transport.
- **No secrets in logs, errors, metrics, or audit records**: authorization headers,
  Provider/Gateway keys, prompts, response bodies, raw source IPs never get logged or
  persisted outside their one-time-response path.
- **Determinism on replay**: random IDs, wall-clock reads, and external results must be
  captured before replay, not regenerated during it.
- **Retry/fallback is bounded** and stops being invisible once downstream response bytes
  are visible to the client — no silent provider-switch mid-stream.
- Don't widen Gemini/Bedrock Beta capability limits or make Azure API versions implicit
  without deliberate contract review — they're pinned on purpose.

Full detail: `.github/copilot-instructions.md` (review checklist used for this repo),
`docs/architecture/threat-model.md`, `docs/contracts/gateway-correctness.md`,
`docs/contracts/audit-integrity.md`.

## Conventions

- Go: run `gofmt` on changed files; prefer stdlib primitives and small interfaces; wrap
  errors with operational context without leaking sensitive values; avoid high-cardinality
  Prometheus labels; keep durable event schemas backward compatible or add a tested migration.
- Commits: short imperative subject, Conventional Commit prefixes encouraged (`feat:`,
  `fix:`, `docs:`, `test:`, `chore:`).
- Never commit local `data/`, `master.key`, `.env`, generated Provider profiles, backups,
  or Provider evidence — several of these already exist locally (`data/`, `master.key`,
  `config.yaml`) and are gitignored; don't fight that.
