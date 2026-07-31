# Heimdall Copilot instructions

Heimdall is a security-first, single-binary LLM gateway. Review changes against
the actual request, failure, persistence, and recovery paths rather than only
the local function being edited.

## Review priorities

Report concrete correctness, security, durability, compatibility, concurrency,
and operability defects. Give each actionable finding a severity of P0, P1,
P2, or P3, cite the smallest relevant line range, and explain the failure
scenario and user-visible consequence. Do not report stylistic preferences as
defects unless they hide a correctness or maintenance risk.

Pay particular attention to:

- authentication or authorization bypasses;
- secret exposure in logs, errors, metrics, audit records, persisted metadata,
  browser artifacts, backups, or Provider requests;
- accounting loss, double settlement, silent refunds, or budget races;
- fail-open behavior during corrupt, unavailable, ambiguous, or stale state;
- crash windows between durable state changes and external Provider calls;
- replay, retry, fallback, idempotency, and revision-conflict behavior;
- unbounded request bodies, streams, queues, retries, allocations, or goroutines;
- request cancellation, timeouts, draining, resource cleanup, and shutdown races;
- OpenAI API, SSE, Admin API, CLI, configuration, backup, and on-disk
  compatibility regressions;
- missing tests for negative, concurrent, crash-recovery, and rollback paths.

## Architectural invariants

- The v1 consistency boundary is one process with exclusive ownership of one
  data directory. Never introduce shared-directory or independent multi-writer
  behavior implicitly.
- The Ledger WAL is the accounting authority. bbolt checkpoints, in-memory
  aggregates, and Parquet files are rebuildable derivatives and must not alter
  Ledger balances.
- A budget reservation must be durable before a Provider can receive a request.
- Attempt settlement atomically releases its reservation and commits cost and
  token usage. An ambiguous Provider outcome is conservatively accounted and is
  never silently refunded.
- bbolt is authoritative for transactional metadata. Auth snapshots, Provider
  registries, and other caches must be versioned or safely rebuildable.
- Authoritative mutations must validate before commit and preserve revision,
  dependency, tombstone, rollback, and audit invariants.
- Future HA foundations use Project as the consistency boundary and epoch-based
  fencing. Standalone remains epoch 1; do not claim that election, affinity, or
  sticky sessions alone provide distributed safety.
- Replaying canonical authoritative facts must be deterministic. Random IDs,
  wall-clock values, and external results must be captured before replay rather
  than generated during it.
- Active HTTP and SSE requests remain owned by one process. Do not claim live
  migration or exactly-once upstream Provider execution.

## Security boundaries

- Provider credentials are encrypted with audience-bound AAD. Gateway keys and
  Admin sessions are stored as one-way representations. Plaintext secrets must
  remain short-lived and must never be returned after their one-time response.
- Logs and errors must not include authorization headers, credentials, Gateway
  keys, webhook secrets, prompts, responses, redaction matches, upstream bodies,
  or sensitive URLs.
- Outbound Provider and webhook traffic must preserve SafeTransport controls:
  HTTPS policy, explicit host allowlists, DNS/IP validation, pinned dialing, no
  environment proxy, and no redirects.
- Public plaintext listeners are rejected by default. Admin and Metrics
  listener exposure must remain fail closed.
- Admin mutations require authentication, Origin/Referer validation, CSRF, and
  revision preconditions. Responses containing one-time material require
  `Cache-Control: no-store`.
- Redaction must remain correct across recursive JSON, tool arguments, and
  chunked streaming boundaries. Never buffer an unbounded stream to simplify
  redaction.

## Gateway and Provider behavior

- Authenticate the Gateway key and enforce Project enabled state, expiry,
  allowed routes, source CIDRs, token limits, RPM, TPM, concurrency, budget,
  Token Guard, and redaction before unsafe upstream work.
- Capability filtering must happen before Provider I/O. A fallback target must
  support every capability requested by the client.
- Retry and fallback are bounded. Once downstream payload bytes are visible, do
  not switch to another Provider or replay the request invisibly.
- Streaming must preserve the first-payload error boundary, bounded SSE parsing,
  downstream write deadlines, cancellation, `[DONE]`, and final settlement.
- Provider errors exposed to clients must be stable and secret-safe. Do not pass
  through arbitrary upstream bodies or headers.
- Azure API versions must remain explicit. Gemini and Bedrock Beta capability
  limits must not be widened accidentally.

## Persistence, recovery, and operations

- Treat write, partial-write, fsync, rename, checkpoint, migration, and recovery
  failures as first-class paths. Durable publication must be atomic and
  retry-safe.
- Preserve exclusive data-directory locking and read-only behavior of diagnostic
  commands.
- Backups must use fixed, mutually consistent metadata, Ledger, Usage, and Audit
  watermarks. Restores must remain explicit, offline, and confirmation-gated.
- Schema and mutation changes require backward-compatibility analysis, migration
  tests, corrupt-input tests, and safe retry after interruption.
- Metrics must be bounded-cardinality and must not expose Project secrets,
  request contents, credentials, or raw source addresses.

## Frontend review

- The React Admin console is a build-time dependency embedded in the Go binary.
  Do not add SSR, a CDN, a service worker, source maps, or browser persistence of
  secrets.
- Preserve in-memory CSRF handling, destructive confirmations, one-time key
  acknowledgement, keyboard/focus behavior, and narrow-viewport usability.
- Check request races and ensure stale responses cannot overwrite newer state.
- Keep the initial gzip bundle within the enforced limit.

## Validation expectations

Require focused regression tests for changed behavior and identify when the
full relevant gates should run:

- `go test ./...`
- `go test -race ./...` for concurrency or lifecycle changes
- `go vet ./...`
- frontend typecheck, tests, build, and embedded-artifact diff for `web/` changes
- compatibility, Provider matrix, stress, soak, backup/restore, or audit checks
  when their contracts are affected

Do not treat a successful build, happy-path unit test, UI state, exporter HTTP
health, or Provider HTTP response alone as proof that authorization,
persistence, accounting, recovery, or database connectivity is correct.
