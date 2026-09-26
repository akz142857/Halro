# ADR 0027: HA physical-replication formats and member handshake

- Status: Accepted
- Date: 2026-09-26
- Decision owners: Halro maintainers
- Supersedes in part: ADR 0004, `docs/architecture/distributed-state-ownership.md`, ADR 0001, and the Standalone-only rows of `docs/architecture/threat-model.md`, exactly as listed in §20 of the HA design

## Context

Halro has four authoritative append paths with already-tested durability and
recovery contracts: Accounting Ledger, Audit, Governance and the metadata
journal introduced by HA Phase 0a. Re-expressing them as semantic mutations
would create a second meaning for every write and would force the Replica to
re-run business logic. Replicating bbolt pages is also invalid: authoritative
and node-local keys share pages, and bbolt exposes neither its dirty-page set nor
a supported replication stream.

The HA design therefore ships the bytes those four stores have already made
durable. This ADR freezes the identities, ordering records, frame envelope,
persistent role state, handshake and projection-recovery choice that Phase 1
implements. It does not add a listener or make a Replica by itself.

## Decision

### 1. Identity and membership

A group has one stable `cluster_id`, one random 128-bit `incarnation` rendered
as `inc_` plus lowercase hexadecimal, and two or three statically configured
members. The incarnation is stable for one Master-Key tenure; a restored whole
cluster or a Master-Key rotation receives a new incarnation. A member has one
stable `node_id`; peers are configured by node ID, dial address and the SHA-256
of the certificate SubjectPublicKeyInfo.

There is no online membership mutation in this protocol version. A configuration
change is an offline operation followed by explicit seeding. A peer that reports
another cluster ID or incarnation is never treated as an empty or new member; it
is refused and requires operator action.

The `replication` configuration block is optional. Absence means Standalone and
creates no cluster state, listener or behavior change. Presence always means
mTLS. There is no `enabled` flag and no plaintext fallback. One configured peer
is valid but emits a warning: the resulting two-member group cannot confirm the
writes that precede Provider I/O after either member is lost.

### 2. Terms, promises and indexes

- `term` is a positive, cluster-wide Primary tenure. It increases on every
  promotion and planned handover; it is never scoped to a Project.
- `promised_term` is the highest prepare a member has durably accepted. A member
  fsyncs it before replying and thereafter refuses frames, acknowledgements and
  prepares below it.
- `replication_index` is a positive, gap-free global order over durable batches
  from the four authoritative stores and protocol control records. Index zero
  means no record.
- `durable_index` is the greatest contiguous index for which both the store
  bytes and the corresponding ordering record are fsynced locally.
- `confirmed_index` is the greatest contiguous index durable locally and
  acknowledged by the configured commit rule in the current term.
- `applied_index` is the greatest confirmed index whose effects and node-local
  projections have been applied by this member.

An index is allocated inside the store commit path after that store's bytes are
durable and before its caller can observe success. The ordering record is then
fsynced. The same index can never be reallocated after restart.

Every new term begins with a `leadership_established` control record. No record
from an older term counts as committed merely because it later exists on a
majority; the current term must first confirm this anchor. This is the physical
replication form of the current-term commit restriction.

### 3. Replication record version 1

The stream is a sequence of length-delimited binary records. Integers are
unsigned big-endian. Decoders reject an unknown version, kind, store, non-zero
reserved field, non-canonical length, zero index/term, `confirmed_index >= index`,
empty identity, excessive length, digest mismatch or trailing byte. A record is
processed only after the whole declared length has arrived.

```text
u32 record_length                 bytes following this field
8B  magic                         "HLRPL001"
u16 version                       1
u16 kind                          1=data, 2=ledger_roll,
                                  3=leadership_established,
                                  4=schema_boundary
u16 store                         0=none, 1=ledger, 2=audit,
                                  3=governance, 4=metadata
u16 reserved                      0
u64 index
u64 term
u64 confirmed_index               sender's confirmed prefix at send time
u64 store_generation              Ledger generation / metadata epoch;
                                  Audit and Governance use 1
u64 store_sequence_first          zero for control records
u64 store_sequence_last           zero for control records
u16 cluster_id_length
u16 incarnation_length
u32 metadata_length
u32 payload_length
32B digest
... cluster_id UTF-8 bytes
... incarnation ASCII bytes
... kind-specific metadata
... payload
```

`record_length` is capped at 16 MiB plus the fixed header. `cluster_id` and
`incarnation` are capped at 64 bytes, metadata at 4 KiB. Data records require a
non-zero store, a non-empty payload and an inclusive sequence range. Control
records require store zero and an empty payload.

The digest is SHA-256 over the domain `halro:replication-frame:v1\x00` followed
by every field from `magic` through `payload`, with the 32-byte digest position
replaced by zeroes. It binds cluster, incarnation, term, index, confirmation
watermark, store generation/sequence, structural metadata and bytes. The ordering journal
stores this digest, so one index cannot acquire different bytes without a
visible fork.

For `kind=data`, payload is exactly the complete original frame bytes already
written by the named store. No decoder translates their contents.
`store_generation` prevents a Ledger generation or metadata-journal epoch from
being silently flattened into one sequence. A metadata epoch change requires a
new authenticated bbolt snapshot and returns `member_requires_full_reseed`;
Ledger generation changes are admitted only through the preceding authenticated
`ledger_roll`. Audit and Governance have no roll and use generation 1. Ledger,
Audit, Governance and metadata-journal integrity checks run before the Replica
writes them.

`ledger_roll` metadata is a fixed binary encoding of generation, first and last
sequence, plaintext length, start/end hash, ending Ledger epoch, plaintext
SHA-256 and UTC seal time in Unix nanoseconds. Compressed length, compressed
checksum and filename suffix are absent because they are node-local.

`leadership_established` has no metadata. `schema_boundary` metadata is two
u32 values, `from_schema` and `to_schema`, and is followed in index order by the
metadata-journal frames that describe the migration.

The cumulative acknowledgement is another length-delimited version-1 binary
record with magic `HLRACK01`. It binds `cluster_id`, `incarnation`, `node_id`,
`term`, `index == durable_index` and `applied_index`. Reserved bytes must be
zero. `index` acknowledges the complete durable prefix through that record;
duplicates at or below the already-confirmed prefix are idempotent, while an
unknown future index, wrong term, wrong identity or non-member is refused.

When that ACK advances the Primary's confirmed prefix, the Primary emits a
length-delimited `HLRCFM01` commit notice binding its identity, term and new
`confirmed_index`. This message is required even when no later data frame
exists: otherwise the last acknowledged frame could remain durable but never
become eligible for Replica apply. Frames and notices have separate queue keys;
the latest notice remains retryable until **every configured Replica** reports
`applied_index >= confirmed_index`, and a Primary restart re-emits the current
term's latest confirmed notice. A disconnected Replica therefore cannot lose
the only notice that makes a final frame applicable. A notice beyond the
Replica's durable prefix is refused; duplicate or older notices are idempotent.

### 4. Ordering journal version 1

`cluster/ordering.journal` starts with a length-delimited header containing
magic `HLRORD01`, version 1, the cluster ID/incarnation, and the four native
store cursors **plus their authenticated chain heads** at replication index
zero. A cursor alone is not a content identity: two independently valid files
can both end at generation 1, sequence 100. The paired head binds the
pre-existing Standalone history or seeded snapshot from which incremental
replication begins; without the cursor recovery cannot distinguish a legitimate
pre-index baseline from a source-fsynced append whose ordering record never
became durable, and without the head it cannot distinguish two different valid
baselines at the same cursor. An HMAC-SHA-256 covers the complete header under domain
`halro:cluster:v1\x00ordering-header\x00`. It then
appends fixed-field records containing:

```text
index, term, confirmed_index, kind, store, store_generation,
store_sequence_first, store_sequence_last, control_metadata,
frame_digest, previous_record_mac, record_mac
```

`record_mac` is HMAC-SHA-256 under the Vault-derived cluster key over the domain
`halro:cluster:v1\x00ordering\x00`, the file identity and every record field
except itself. `previous_record_mac` forms a chain. A record is durable only
after the journal fsync succeeds. Partial tails are truncated to the last whole
authenticated record; a whole-record suffix missing behind the index/head
recorded in `state.json` is corruption, not a shorter valid history.

The file never contains payload bytes or secrets. It carries only bounded,
non-secret control metadata plus an order and digest index over payload bytes
owned by the four stores. Persisting the original confirmation watermark and
control metadata makes an unconfirmed frame's exact envelope reconstructible.

### 5. Persistent member state version 1

`cluster/state.json` is a single JSON object published by write-temp, file
fsync, rename and directory fsync. It contains:

```json
{
  "version": 1,
  "cluster_id": "production-a",
  "incarnation": "inc_...",
  "node_id": "halro-0",
  "role": "primary",
  "term": 7,
  "promised_term": 7,
  "durable_index": 10241,
  "applied_index": 10241,
  "ordering_head_mac": "sha256:...",
  "projection": {"index": 10241, "metadata_epoch": 4, "metadata_sequence": 91},
  "peers": [{"name": "halro-1", "address": "...", "spki_sha256": "sha256:..."}],
  "mac": "sha256:..."
}
```

The MAC is HMAC-SHA-256 under the same cluster key with domain
`halro:cluster:v1\x00state\x00`. Its input is a versioned, length-prefixed binary
encoding of the fields above excluding `mac`; peers are ordered by name. It is
not the JSON rendering, so whitespace or object-key order cannot change the
authenticated meaning. Unknown fields and duplicate peer identities are
refused. `role` is one of `primary`, `replica` or `awaiting_decision`.

The cluster key is derived from the unlocked Master Key with HKDF-SHA-256,
incarnation as salt and `halro:cluster:v1` as info. It is never stored or sent.
An offline Master Key rotation therefore starts a new cluster incarnation and
new ordering/state files while both old and new keys are still available; all
Replicas are then re-seeded. Reusing the old incarnation after rotation is
forbidden because the new key could not authenticate its files. Phase 2 owns
that staged publication; until it exists, every offline data mutation command,
including `halro key rotate`, refuses a configuration containing `replication`.

### 6. Member handshake version 1

The transport is TLS 1.3 mutual authentication. The presented chain must verify
against `replication.tls.ca_file`; the authenticated peer node ID must be in the
static peer list; and the SHA-256 of its SPKI must equal that peer's pin. Passing
the CA check but failing the pin is a refusal.

After TLS, both sides exchange bounded, length-delimited version-1 binary
`hello` messages with magic `HLRHEL01`. They contain cluster ID, incarnation,
node ID, role, term, promised term, durable/applied index, a fresh non-zero
32-byte nonce, and a `(current, minimum-readable, maximum-readable)` triple for
each compatibility domain:

- binary version;
- replication protocol version;
- bbolt schema;
- Ledger frame epoch;
- metadata-journal frame version.

Each current value must lie inside its own range. There must be a non-empty
intersection for every pair, and each side's current value must fit the other
side's readable range. An incompatible peer is refused before it can receive or
acknowledge a frame. Accepted nonces enter a bounded per-peer replay cache;
running without that cache is itself refused.

Possession of a certificate is not enough. Each side derives the TLS exporter
`EXPORTER-Halro-Replication-v1` with a context equal to the SHA-256 of both
length-prefixed complete hellos, then
returns HMAC-SHA-256 under the cluster key over
`halro:cluster:v1\x00handshake\x00`, that exporter, both nonces and both complete
hellos. Client and server direction bytes differ. Proofs use constant-time
comparison. No Master Key fingerprint is exchanged or accepted as proof.

### 7. Projection rollback chooses path A-prime

This ADR selects §11.2 path A′. A former Primary whose unconfirmed suffix must
be removed does not mutate bbolt checkpoints backwards and does not maintain
periodic local bbolt rollback snapshots. It closes all stores, truncates the
append-only stores at the accepted frame boundary, and downloads an authenticated
bbolt snapshot from the new Primary at a stated confirmed index `C'`.

The snapshot carries cluster/incarnation, schema, metadata epoch/sequence,
confirmed index, file length and SHA-256. It is written under `staging/`, checked
against the Master-Key handshake and digest, then atomically published. The
member remains unready while the other stores catch up to `C'`; normal
incremental replication resumes after that common point. No placeholder object
or partial projection becomes visible.

A′ is chosen because the measured bbolt projection is small relative to the
Ledger and because it leaves all existing monotonic-checkpoint guards intact.
Path A would add snapshot scheduling, retention and another authenticated head
to every member solely to optimize an exceptional rollback.

## Superseded contracts

- ADR 0004 remains authoritative for the names Standalone, HA and Cluster, and
  for Project as the future sharding boundary. Its Phase 0 semantic-mutation,
  ownership-epoch and deterministic-replay requirements do not govern one HA
  group. Physical frame prefixes, cluster terms and durable promises replace
  them.
- ADR 0004's statement that an old leader cannot start a Provider call after a
  newer ownership epoch is narrowed: promises prevent further confirmation;
  operator fencing plus conservative unknown-outcome recovery covers the
  residual between the last local check and socket write.
- `distributed-state-ownership.md` invariants 1, 3 and 5 are superseded for HA:
  term is cluster-wide, a durable promise replaces a stale ownership token, and
  frame-prefix equivalence replaces semantic replay. They remain design inputs
  for future multi-shard Cluster mode.
- ADR 0001's original bbolt authority was already replaced by Phase 0a: bbolt is
  the projection of the metadata journal plus node-local keys.
- The Standalone-only trust statements in `threat-model.md` do not describe an
  HA group. Phase 2 must publish the expanded member/PVC/certificate boundary
  before HA is released.

## Consequences

- Phase 1 can implement transport and persistence without inventing format at
  the call site.
- The repository implementation includes bounded codecs, the mTLS/SPKI helper,
  authenticated ordering/state persistence, cumulative ACK aggregation and
  Primary/Replica protocol state machines. Those primitives do not create a
  listener or intercept any store commit until Phase 1 runtime wiring does so.
- Every acknowledged batch pays for the store fsync and ordering-journal fsync;
  batching may amortize but never bypass either durability point.
- A member compromise exposes the shared data and Master-Key domain. mTLS and
  SPKI pinning authenticate members; they do not make a compromised member
  honest.
- The format supports two or three members only. Changing that requires a new
  protocol version and an explicit quorum rule, not a larger peers list.
- Standalone remains byte-for-byte and behaviorally unchanged when the optional
  configuration block is absent.
- A Primary restart may find an authenticated ordering suffix above its last
  known confirmed prefix. Startup must reconstruct the exact encoded frames from
  the four stores, match every term/watermark/metadata/digest/generation/sequence
  to the ordering records, and requeue them. Missing or mismatched suffix bytes fail closed;
  they are neither discarded nor promoted to confirmed.

## Required verification

- byte-for-byte golden fixtures for every record kind and both cluster files;
- unknown version/kind/store, length overflow, trailing bytes and digest/MAC
  mismatch all fail closed;
- partial ordering tail recovery and whole authenticated suffix deletion are
  distinguished;
- an index cannot be accepted with a different digest;
- a commit notice remains retryable until every configured Replica has reported
  the notice's index applied;
- CA-valid but SPKI-mismatched peers, wrong cluster/incarnation, replayed nonce,
  wrong Master Key and incompatible ranges are refused;
- two-node configuration warns, while absent configuration creates no listener,
  file, command or behavior delta;
- A′ publication interruption leaves the previous database intact, and a node
  cannot become ready until every authoritative store reaches the snapshot's
  confirmed index.
