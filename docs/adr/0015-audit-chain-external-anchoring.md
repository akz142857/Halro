# ADR 0015: External anchoring for the audit chain

- Status: Accepted and implemented 2026-08-06 — default sink is A (dead-man
  probe pull). See "Implementation notes" at the end for what shipped.
- Date: 2026-08-06
- Source: `docs/review/260805/260805.md` §八 S-P0-2, adversarial verdict PARTIAL
  (file mode CONFIRMED, `key_slots` mode REFUTED); tracked as P1-7 in
  `docs/review/260805/progress.md`.

## Context

The audit log is a hash chain: every frame carries an HMAC over the previous
hash, and `Log.Summary()` reports `{Records, LastHash, Bytes}`. On startup and
around every backup, `reconcileAuditCheckpoint` compares that summary against
the checkpoint persisted in bbolt and refuses to run when they disagree. This
detects truncation and any edit that does not recompute the chain.

It does not detect an edit made by someone who can recompute the chain. In
`storage.master_key.mode: file` — the default — the HMAC key is
`HKDF(master.key)` (`internal/vault/audit.go`), `master.key` sits on disk, and
the checkpoint sits in bbolt inside the same data directory. One principal
therefore holds the key, the log, and the record that the log is intact. That
principal is also the one the log exists to hold accountable. They can forge an
event sequence, recompute every hash, rewrite the checkpoint, and leave nothing
behind that the process can notice, because **nothing about the chain has ever
been recorded outside the data directory**. The dead-man probe already
transmits off-host, but its heartbeat carries liveness only — no audit state.

`mode: key_slots` does not have this property: no plaintext master key exists on
disk, deriving the audit key requires KMS credentials, and each use of the key
leaves a record in the KMS provider's own log. The adversarial review confirmed
the file-mode attack and refuted the key-slots one. So the gap is not "the audit
design is weak" — it is that the default deployment mode has no witness.

An external anchor does not prevent the rewrite. It makes the rewrite
*detectable*: if `(Records, LastHash)` was observed by something the attacker
does not control, a later chain that disagrees with that observation at the same
sequence is proof of tampering, and one whose record count has gone backwards is
proof of truncation.

## Decision

### The anchor record

Heimdall periodically emits an **anchor**: the current `Records`, `LastHash`,
`Bytes`, an anchor sequence that never decreases, the instance identity, and the
wall-clock time of the observation. It is derived entirely from
`audit.Log.Summary()`, contains no event payloads, and is therefore safe to send
somewhere with weaker confidentiality than the audit log itself. Anchors are
emitted on a fixed interval and immediately after any append that advances the
chain past a configured record delta, so a burst of activity cannot slip between
two ticks.

### Verification is a first-class operation

An anchor nobody checks is decoration. Heimdall ships a verification path that
takes a set of previously emitted anchors and the current log, and reports, per
anchor: the chain agrees, the chain disagrees at that sequence (tampering), or
the chain is shorter than the anchor (truncation). Startup reconciliation gains
the anchors it can reach locally; the full check is an explicit operator
command, because the authoritative copy of an anchor lives somewhere Heimdall
deliberately cannot rewrite and may not be able to read.

### Emission is fail-open, and says so

Audit *appends* are fail-closed: an event that cannot be recorded stops the
operation. Anchor emission is deliberately **not**. An unreachable anchor sink
must not take the gateway down — that would hand any principal who can partition
one syslog host a total outage, which is exactly the reversal the review found in
the "unauthenticated flood → fail-closed backfire" chain. A failed emission is
retried, counted, and surfaced as a health signal; it does not block appends.

### File mode is documented as the weaker mode

The configuration reference and the security guide state plainly that in `mode:
file` the audit chain is tamper-*evident against outsiders* and only
tamper-*evident against the operator* once anchors are enabled and checked, and
that `mode: key_slots` is the precondition for non-repudiation. This costs
nothing and is the single highest-value part of this ADR.

## Decided: default sink

Where anchors go is a deployment decision, not an implementation detail: each
option below buys a different amount of independence at a different operational
cost. The mechanism is destination-agnostic — the sink is a small interface with
one method — so B and C stay available as configured alternatives; **A ships as
the default** for the reason under "For" below: it is the only option that adds
no new dependency and no new credential to a deployment that has installed
nothing beyond the product itself.

### A. Dead-man probe pulls the summary (decided default)

Expose the anchor on an authenticated endpoint and let the existing
`heimdall-deadman` probe record it in its own append-only audit file. The probe
already runs as a separate binary, on a separate host, with a separate token and
a pinned CA, and already writes its own audit file — this reuses infrastructure
the product ships and an operator following the runbook already deployed.

- **For:** no new dependency, no new credential, no new network path. The
  witness is off-host by construction.
- **Against:** the witness is only as independent as the dead-man host. An
  attacker who owns both hosts owns both records. It is a genuine improvement
  over "no witness anywhere", not a compliance-grade one.

### B. Syslog (RFC 5424 over TLS)

Emit each anchor to a remote syslog collector.

- **For:** almost every deployment that cares about audit already has a log
  collector, and it is usually already write-once from the sender's point of
  view.
- **Against:** delivery is best-effort by nature; retention and immutability are
  entirely the collector's business, and Heimdall cannot verify either.

### C. S3 Object Lock in compliance mode

Write each anchor as an object under a retention policy that even the account
root cannot delete before expiry.

- **For:** the only option here that is durable against the operator themselves,
  which is the actual threat.
- **Against:** a cloud dependency and a second set of credentials in a product
  whose stated philosophy is a self-contained single binary. Best paired with
  `mode: key_slots`, which already assumes a cloud KMS — a deployment that has
  accepted one has usually accepted the other.

### Decided

1. A ships enabled by default; B and C are configurable alternatives. More than
   one sink may be configured at once — anchors are cheap and idempotent to
   emit, and an operator moving from A to C during a migration needs both live
   for the overlap.
2. Default anchor interval: 5 minutes. Default record delta: 500 — chosen so a
   burst of settlement traffic still anchors well inside the interval instead
   of waiting for the tick. Both are operator-configurable.
3. `mode: file` *warns* at startup when no anchor sink is configured, the same
   posture as the Developer Workbench reachability warning this repo already
   ships (P2-13/P1-10): the gap is real, the default stays usable, and the
   person who can fix it gets told.

## Rejected alternatives

### Derive the audit HMAC key from something other than the master key

Rejected. Any key material Heimdall can reach unaided is reachable by whoever
holds the data directory; moving the derivation only moves the problem. What is
missing is a second party, not a second key.

### Anchor into the ledger, or any other file in the data directory

Rejected for the same reason the checkpoint fails: both objects are inside the
blast radius. Verification requires a copy the attacker cannot rewrite.

### Make anchor emission fail-closed like audit appends

Rejected. See above: it converts a reachability problem into an availability
weapon, and the review already documented that exact reversal.

### Declare the problem solved by recommending `mode: key_slots`

Rejected as the *whole* answer, though it is part of it. `file` is the default,
is what a single-host deployment will keep using, and is what the quickstart
produces. A recommendation that only helps people who leave the default is not a
fix for the default.

## Consequences

- File-mode deployments gain a detection story where they had none; they do not
  gain prevention, and the documentation must not imply otherwise.
- Anchors are a new externally visible artifact whose format becomes a
  compatibility surface once a verifier reads historical ones.
- Verification is an operator action with an operator's judgement attached: a
  disagreeing anchor is evidence, not an automatic failure mode, and Heimdall
  will not refuse to start over one it cannot independently confirm.

## Required verification

- an anchor emitted, the log rewritten with a recomputed chain and checkpoint,
  and verification reporting disagreement at the anchored sequence;
- the same with events removed, reporting truncation rather than disagreement;
- an unreachable sink proven not to block audit appends or gateway traffic, with
  the failure surfaced as a health signal and retried;
- anchor sequence proven monotonic across restart, so a restart cannot be used to
  replay a stale anchor as current;
- `mode: key_slots` documented and tested as the non-repudiation precondition,
  including that the audit key cannot be derived without KMS access.

## Implementation notes (2026-08-06)

- Instance identity: no such concept existed anywhere in the codebase before
  this. Decided in favor of generating and persisting a UUID-shaped ID in
  bbolt at first start (`Store.SeedInstanceID`) over asking the operator to
  set one in config — it removes a cross-instance coordination burden for
  fleets, at the cost of the ID being meaningless until an operator looks it
  up (acceptable: it only has to disambiguate anchors, never be memorized).
- Anchor storage: a bounded bbolt ring (`bucketAuditAnchors`, retains the most
  recent 1000 — a little over three days at the default 5-minute interval)
  serves two purposes at once — the endpoint reads it, and it is also where
  local reconciliation *could* read from, though this ADR's local-startup
  reconciliation was scoped down to nothing new: `reconcileAuditCheckpoint`
  already provides a stronger local guarantee than comparing against
  locally-stored anchors would (both live inside the same blast radius), so
  the anchor ring's value is entirely in being read by an off-host puller.
- Emission cadence: `runAuditAnchorMaintenance` polls every 10s (a fixed
  `var`, not tied to config) and emits when the configured interval has
  elapsed *or* the record delta has been crossed since the last emission —
  an approximation of "immediately after a qualifying append" that does not
  require hooking every audit append call site. 10s against a default 5m
  interval is a fine enough grain that this reads as immediate in practice.
- Endpoint: `GET /audit/anchors?since=<seq>` added to the existing metrics
  listener (`internal/app/audit_anchor.go`), authenticated by a *new*
  independent bearer-credential domain reusing `internal/metricsauth`
  wholesale — a second file path is enough to get an independently rotated
  credential without cloning the package; the rotation/audit/revocation
  machinery is identical to metrics' own, just pointed at a different file.
- Dead-man side: `TargetConfig.AnchorURL` (optional, `heimdall`-kind targets
  only) reuses that target's existing `BearerTokenFile`/TLS — the operator
  points the same credential file at both the health check and the anchor
  endpoint, which means syncing the anchor credential's active token into
  that file is an operational step this ADR does not automate.
  `Engine.Checker`'s signature (latency + reason only) could not carry a
  payload, so anchor pulling is a second, parallel unlocked step in `Tick`
  (`pullAnchors`), not a reuse of the probe abstraction — matching what the
  Explore phase flagged before implementation started. Pulled anchors persist
  to a JSON-lines file (`anchorWriter`, mirroring the existing `auditWriter`
  shape) and the per-target high-water mark lives in the same `TargetState`
  the probe already persists, so a restart resumes exactly where it left off.
- Verification: `heimdall audit verify-anchor --anchors <file>` decodes that
  JSON-lines file and, for each anchor, replays the local audit chain and
  compares the record at that historical sequence — agree / disagree
  (tampering) / truncated (the anchor claims more records than exist now).
- What shipped narrower than the open decision anticipated: sinks B (syslog)
  and C (S3 Object Lock) are reserved config values that fail validation with
  "not implemented yet" rather than working alternatives — the config schema
  does not need to change again when they land, but only A exists today.
