# ADR 0016: Cryptographic integrity for Ledger frames

- Status: Accepted — epoch 4 ships before the first tag; chain verification
  failure is fail-closed; the v1/v2/v3 prefix reports as checksum-only and
  continues. Decided 2026-08-06.
- Date: 2026-08-06
- Source: `docs/review/260805/260805.md` §八 中低危 ("ledger 只有 CRC32 无密码学完整性");
  tracked as P2-16 in `docs/review/260805/progress.md`.
- Builds on: [ADR 0014](0014-ledger-wal-backup-compatibility.md) (frame epochs,
  backup manifest), [ADR 0015](0015-audit-chain-external-anchoring.md) (what a
  local integrity guarantee can and cannot claim).
- Amended 2026-08-06: `feat/timezone-governance` shipped the same day and took
  frame epoch 3 for an unrelated purpose (period identity, P2-16 tracking in
  `docs/review/260805/progress.md`). This ADR originally proposed epoch 3 for the MAC/chain; every
  epoch reference below now reads 4, and v3 joins v1/v2 as permanently
  checksum-only. No other part of the mechanism changed.

## Context

Every accounting fact Halro has — what was reserved, what was committed,
which price snapshot applied, what a project spent today — is replayed from the
Ledger WAL at startup. A frame is 24 bytes of header plus a JSON payload,
protected by CRC32.

CRC32 detects accidental corruption: a torn write, a bad sector, a truncated
tail. It detects nothing deliberate. It is a public, keyless, linear function,
so anyone who can write to `usage.wal` can change `committed_micros_usd` in a
settled frame, recompute the checksum in one line, and the gateway will replay
the forged number as fact — under budget limits, into the daily balance, out
through the usage API and into whatever the operator bills from.

The Audit log next to it takes the opposite position: HMAC-SHA256 per record
plus a hash chain plus a checkpoint in bbolt, with the key stored encrypted in
the Vault. Two append-only logs in the same data directory, holding facts of
comparable consequence, protected to entirely different standards. The asymmetry
is the finding — not that CRC32 is a poor checksum, which it is not.

What this can and cannot buy is worth stating before choosing a mechanism, and
ADR 0015 already had to state it for Audit: a MAC whose key lives on the same
host defends against **write access without key access** — a compromised backup
job, a container escape into a mounted volume, an operator with filesystem
access but not the Master Key, a restored file swapped in from elsewhere. It
does not defend against someone holding `master.key` in `mode: file`, who can
recompute anything they like. That is a real and common threat model, and it is
also exactly the boundary this ADR must not overstate.

## Decision

### Frame epoch 4

Epoch 3 is already spoken for. `feat/timezone-governance` (merged 2026-08-06,
same day as this ADR) shipped `frameVersionPeriod = 3` to carry a period's own
zone, version and UTC bounds in the payload, so a charge can be re-derived
without consulting a setting (`internal/ledger/log.go`). Those frames have no
MAC and were written before this ADR's key existed; the rule below — existing
bytes are never rewritten — applies to them exactly as it applies to v1 and v2.
The guarantee this ADR adds has to start one epoch later than planned.

`HLDG` frame version 4, following the rules ADR 0014 already set: existing bytes
are never rewritten, a reader accepts v1, v2, v3 and v4 in one file, an
unsupported epoch is `ErrUnsupportedVersion` and not `ErrCorrupt`, and the
bbolt compatibility gate records the minimum reader version before the first
v4 append. v4 is additive on the header only — it carries the same period
identity payload v3 introduced, plus the MAC and chain hash described below.

The header grows to carry a 32-byte MAC and the 32-byte chain hash of the
previous frame. The CRC32 field stays exactly where it is and keeps its meaning:
it is the cheap check that distinguishes a torn tail from a corrupt frame during
recovery, and it answers before the MAC is computed. A frame that fails CRC in
the tail position is still a partial write; a frame that passes CRC and fails
its MAC is tampering, and the two must not collapse into one error.

### The chain, and why sequence numbers are not enough

Each v4 frame carries `MAC = HMAC-SHA256(key, header-without-mac || previous-hash
|| payload)`, and `hash = SHA256(MAC)` becomes the next frame's `previous-hash`.

A per-frame MAC alone would leave deletion undetected. The scanner rejects a
sequence that goes backwards but accepts a gap, so removing a middle frame — the
one settlement that costs the most, say — produces a file that still verifies
frame by frame. The chain is what makes deletion, reordering and splicing
detectable, and it is the same construction Audit already uses; two different
answers to one question in one data directory is the state this ADR is trying to
leave.

The chain head (sequence, offset, hash) is checkpointed into bbolt the way the
Audit summary is, so a truncation that removes the newest frames is detected at
startup rather than silently accepted as "the log is shorter today".

### Key handling follows Audit exactly

A 32-byte key generated once, stored encrypted in the Vault, loaded at startup,
with derivation from the Master Key only as the bootstrap path for an instance
that has none yet — the shape `loadAuditHMACKey` already implements. This is not
a stylistic choice: deriving the key from the Master Key on every open would
make Master Key rotation silently invalidate every historical frame, because
rotation changes the derivation input. An encrypted key that rotation re-wraps
keeps old frames verifiable across a rotation, which is the whole point of
retaining them.

Domain separation: `halro:ledger:v1` / `halro:ledger-hmac-key:v1`, distinct
from the Audit strings, so one key can never verify the other's frames.

### The guarantee starts where v4 starts

v1, v2 and v3 frames stay CRC-only forever, because rewriting them would
destroy the byte history that ADR 0014 exists to protect. v3 sits in this set
for the same reason as v1 and v2, not because it is old — it is CRC-only
because it was written before this ADR's key existed, same as the others.
Verification therefore reports three states, and the operator has to be able
to tell them apart: *authenticated* (v4, MAC verified, chain intact),
*checksum-only* (v1/v2/v3, integrity as good as CRC32 and no better), and
*failed*. The backup manifest records the sequence at which the chain begins,
so a restored archive cannot be mistaken for one that is authenticated end to
end.

### Cost

HMAC-SHA256 over a frame of a few hundred bytes is on the order of a
microsecond; the append path's cost is dominated by `fsync`, by orders of
magnitude. Verification of the whole chain happens at startup replay, where it
adds a hash per frame to work that already parses JSON per frame. This is not a
performance decision, and the ADR should not pretend to trade anything away
here.

## Decided

1. **Worth a format epoch before the first tag: yes.** No release exists yet, so
   epoch 4 costs nothing but the code today and a migration note forever
   afterwards is the alternative. This is more urgent than when the ADR was
   first drafted: epoch 3 shipped in the meantime for an unrelated reason
   (period identity), so v1, v2 and v3 are already permanently unauthenticated
   before a single tag exists — shipping epoch 4 now stops a fourth
   pre-authentication epoch from accumulating the same way.
2. **The checkpoint gates startup: yes, fail-closed.** Same posture as Audit's
   `reconcileAuditCheckpoint` — a tampered or truncated WAL refuses to serve
   rather than replaying a number nothing vouches for. This is a new way for the
   gateway to be down, and that is accepted as the correct trade for an
   accounting log under this repo's fail-closed house style.
3. **The v1/v2/v3 prefix reports as checksum-only and continues**, mirroring the
   three-state report already specified above (authenticated / checksum-only /
   failed). No operator acknowledgement gate — that raises the bar to what a
   compliance deployment wants, and nothing here claims compliance-grade
   provenance for history that predates the key.

## Rejected alternatives

### Add the MAC under an existing frame version

Rejected for the reason ADR 0014 already gave for v2: an old reader would treat
a compatible upgrade as corruption, and corruption triggers repair paths. The
same reasoning rules out piggybacking onto v3 as well — it shipped its own
payload contract (period identity) before this ADR's key existed, and a v3
reader that predates the MAC would reject the extra field the same way a v1
reader rejects v2's.

### Rewrite the whole WAL into an authenticated file

Rejected. It changes historical bytes, CRCs, sequences and every backup's
evidence, to buy a guarantee about a period during which no key existed. A log
whose history was rewritten to prove it was never rewritten is not evidence.

### Per-frame MAC without a chain

Rejected: it leaves deletion of a middle frame undetectable, and deleting the
expensive settlement is the obvious attack on an accounting log.

### Sign frames with an asymmetric key so a verifier needs no secret

Rejected for this milestone. It is the stronger construction and it pairs
naturally with the external anchoring in ADR 0015, but it needs key custody,
rotation and distribution answers that ADR does not have yet. If anchoring lands
with a real external witness, this is worth revisiting as one design rather than
two.

### Rely on the Audit log to catch Ledger tampering

Rejected: they record different facts. Audit records that an administrator
changed a price; it does not record what a request settled at. Nothing in Audit
would notice `committed_micros_usd` changing under it.

## Consequences

- A fourth frame epoch, permanently, in a reader that must keep accepting all
  four — three of them (v1, v2, v3) forever unauthenticated, one already
  written in production before this ADR's key exists anywhere.
- Backup manifests and the compatibility gate gain the chain head; restore gains
  a verification step that can fail on evidence rather than on format.
- Integrity becomes reportable per range rather than per file, and the
  documentation has to say plainly that the guarantee starts at a sequence
  number and that `mode: file` bounds what it means at all.
- The version-selection logic in `eventFrameVersion` currently branches by
  event shape (legacy vs. v2 accounting fields vs. v3 period identity); once
  v4 ships it must collapse to a single writer epoch — every new frame has to
  be v4, or the authenticated range would have silent gaps by event kind
  rather than only by sequence.

## Required verification

- byte-for-byte v1/v2/v3 fixture preservation, and mixed v1/v2/v3/v4 replay;
- a forged payload with a recomputed CRC rejected by the MAC — the exact attack
  this exists to stop;
- a deleted middle frame rejected by the chain, and a truncated tail still
  reported as a partial write rather than tampering;
- Master Key rotation followed by successful verification of frames written
  before it;
- a restored backup whose manifest chain head disagrees with the file, failing
  restore rather than starting;
- verification reporting the v1/v2/v3 prefix as checksum-only rather than as
  authenticated;
- a v3 (period-identity) event written by the pre-ADR code path replays
  correctly under the v4-aware reader, with its period fields intact and no
  MAC expected.
