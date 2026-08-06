# ADR 0015: External anchoring for the audit chain

- Status: Proposed — the mechanism is decided below; **which destination ships as
  the supported default is not, and is left to the maintainers** (see "Open
  decision").
- Date: 2026-08-06
- Source: `docs/review/260805.md` §八 S-P0-2, adversarial verdict PARTIAL
  (file mode CONFIRMED, `key_slots` mode REFUTED); tracked as P1-7 in
  `docs/review/progress.md`.

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

## Open decision

Where anchors go is a deployment decision, not an implementation detail: each
option below buys a different amount of independence at a different operational
cost, and the answer depends on what the deployment already runs. **This ADR
does not pick one.** The mechanism above is destination-agnostic; the sink is a
small interface with one method.

### A. Dead-man probe pulls the summary (recommended as the default)

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

### What has to be decided

1. Which of A/B/C ships enabled by default, and whether more than one sink may
   be configured at once.
2. The default anchor interval and record delta.
3. Whether `mode: file` should *warn* at startup when no anchor sink is
   configured, or stay silent.

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
