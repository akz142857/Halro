# Deep review: ledger, budget, auth (2026-08-22)

Method: 8 finder agents (3 lenses × ledger, 3 × budget, 1 × auth full-read,
1 × cross-package accounting seam), every finding adversarially verified by an
independent agent instructed to refute it. 14 agents, ~1.2M tokens, 387 tool
uses. Both blocking findings were additionally spot-checked by hand against
the cited source lines, and the ledger finding was **live-reproduced twice
independently** (finder and verifier, same corruption at offset 752).

Scope: `internal/ledger`, `internal/budget`, `internal/auth`, and the
accounting seams into `internal/gateway` / `internal/app` / `internal/usage`.
All findings pre-exist v0.2.0 unless noted — none were introduced by the
v0.2.0..HEAD range.

## Confirmed findings

### 1. BLOCKING — interrupted Replay corrupts the WAL via the shared file cursor

`internal/ledger/log.go:405` (Replay), `:546` (writeBatch), `:807` (scan)

`Log.Replay` scans `l.file` in place; `scan` seeks to the start offset once
and advances the **shared OS file cursor** with stateful reads. When the visit
callback returns an error mid-scan (a context timeout is enough), `scan`
returns immediately without restoring the cursor, and — by design, per the
ADR-0016 change — a caller-visit error does not mark the ledger for recovery,
so Appends remain allowed. `writeBatch` contains **no Seek at all**: it writes
wherever the cursor happens to be. After an interrupted Replay that is
mid-file, so the next Append **silently overwrites already-durable,
already-acknowledged frames**. Append and fsync both report success; the
damage surfaces only at the next process restart, when the chain scan hits the
clobbered bytes and refuses to open (`ErrTampered` →
AccountingRecoveryRequired).

Production trigger exists today: `usage.Collector.CatchUp`
(`internal/usage/collector.go:80`) replays through this exact interface with a
per-record `ctx.Err()` check, wired to the **same live `*ledger.Log`** the
gateway appends to (`internal/app/runtime.go:346`), called periodically under
a 10-second timeout (`runtime.go:866`, `:914`). A WAL large enough or a disk
slow enough that catch-up exceeds 10s corrupts the ledger with no crash, no
attacker, and no error surfaced at the time — and takes the gateway down on
its next restart.

Reproduced: append evt_1..3 (file 1128 bytes) → Replay with visit erroring on
record 2 (cursor left at 752) → Append evt_4 returns nil error, file size
stays 1128 (evt_3 physically overwritten) → reopen fails: "chain link does not
match the running hash" at offset 752.

The existing test `TestCanceledVisitDoesNotCondemnTheLedger` asserts only that
Append returns nil after a canceled Replay — it never reopens the file, which
is exactly the gap that let this ship.

Fix directions (either suffices; both are small): `writeBatch` seeks to
`l.offset` before writing, or `Replay` restores the cursor on every exit path.
The regression test must reopen the log after the interrupted-replay-then-
append sequence.

### 2. BLOCKING — unordered auth snapshot refresh can resurrect a revoked key

`internal/auth/snapshot.go:70` (Refresh), `internal/app/activation_state.go:211`

`Snapshot.Refresh` reads keys/projects from the store and then
`s.current.Store(next)` — last-writer-wins, no revision/ordering guard. The
background activation-recovery goroutine calls `r.activateAuthSnapshot()`
**without a lock** (unlike the topology activation two lines above it, which
takes `adminTopologyMu`). Interleaving: recovery's Refresh reads the store
*before* an admin revokes a Gateway Key; the admin mutation commits and its
own Refresh installs the post-revocation snapshot; the recovery goroutine's
older read then completes and stores the **pre-revocation snapshot over it**.
The revoked key authenticates again until the next refresh. Fail-closed is
inverted: a revocation the operator watched succeed is silently undone.

Fix directions: guard Refresh with a mutex (matching the topology pattern), or
carry a store revision into the snapshot and refuse to install an older one.

### 3. MAJOR — streaming settlement charges the fixed per-request fee on never-sent requests

`internal/gateway/service.go:2591` (seam/accounting)

For streaming attempts that fail before any byte reaches the provider (DNS/
TCP connect failure), settlement still commits `FixedRequestMicrosUSD` for
deployments priced with a fixed per-request fee. Conservative accounting is
for *ambiguous* outcomes; a connect failure is definitively unsent, and the
non-streaming path treats it as such. Operators with fixed-fee pricing
overcharge projects on every connect failure.

### 4. MAJOR — native Anthropic Messages endpoints mismap fatal pre-provider errors to 502 provider_error

`internal/gateway/service.go:1212` (seam/accounting)

`MessagesNative` funnels fatal `startAttempt` errors (Token Guard block 403,
accounting-unavailable, budget exhaustion) through `exhaustedAttemptsError` →
`mapProviderError`, so the client sees a generic 502 `provider_error` instead
of the real 403/policy code. The OpenAI-facade paths map these correctly. A
policy block reads as a provider outage; clients retry what should never be
retried.

### 5. MINOR — Settle never bounds committed cost by the attempt's reservation

`internal/budget/manager.go:567`

Settlement commits whatever the provider response cost, even when it exceeds
the amount reserved for that attempt — a provider returning materially more
billable tokens than `PreparedOutputTokens` estimated can drive a project past
its budget ceiling in the settling write (subsequent admissions then refuse,
but the overshoot has already been committed). Correct behavior is arguable
(the tokens were genuinely consumed); recording as a known, deliberate
overshoot path that deserves a stated bound or an explicit comment.

## Refuted (1)

- `internal/budget/manager.go:765` — claimed TOCTOU between `applyFailure()`
  check and `m.log.Append`. The window exists, but the verifier established
  the consequence claimed (a durable record for a refused attempt reaching
  aggregates) cannot occur: replay-side handling reconciles it. Kept here so
  the next reviewer does not re-litigate it from scratch.

## Consequence for v0.3.0

Findings 1 and 2 pre-exist v0.2.0; the release range did not introduce them.
But both are in the blocking class the assessment procedure names (silent
data loss; auth fail-open). Recommendation: fix #1 and #2 before tagging
v0.3.0 — both fixes are small and testable — or record an explicit owner
decision to ship with them known. Findings 3–5 can be issues.

## Resolution (2026-08-22, same day)

- Finding 1 fixed in PR #207 (`fix(ledger)`, merged): writeBatch seeks to the
  tracked tail before every write; regression test reopens the file after
  interrupted-replay-then-append and fails without the fix at the reproduced
  chain break (offset 752).
- Finding 2 fixed in PR #208 (`fix(auth)`, merged): Refresh serialized end to
  end; regression test drives the stale-read-installs-last interleaving with
  a gated store stub and fails without the mutex.
- Findings 3–5 filed as issues #210 (fixed fee on never-sent streaming
  attempts), #211 (native Messages 502 mismap), #212 (Settle unbounded by
  reservation).
