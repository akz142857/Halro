# Read-only ordering inspection

`replication.InspectOrderingJournal` is a bounded-memory prerequisite for offline
verification. The live/restart `OpenExistingOrderingJournal` uses a writable
descriptor, file/directory sync, and partial-tail repair; offline verification
must not use that path.

The caller holds the exclusive data-directory lock and obtains the expected
durable ordering index/MAC and cluster/incarnation from authenticated member
state. It supplies a role-allowed `through` index no greater than that durable
index. The inspector opens only an existing regular file, captures its descriptor
and length, and checks the bounded authenticated header and every fixed-size
record in the captured file. It retains only four store cursors and scalar cuts.

`OrderingInspection.Header` preserves the authenticated index-zero store
positions **and heads**; a seeded index-zero baseline is not genesis.
`Allowed` identifies the requested cut; `Durable` preserves the authenticated
member's expected-index cut and term; `Head` identifies the actual physical
journal tail. A physical journal ahead of member state is allowed only after its
entire MAC chain and semantic transitions validate. It never advances `Allowed`.
The trusted member MAC must match at its exact expected index.

Index/term/leadership transitions, native store ranges and Ledger roll transitions
validate with the same cursor reducer as the live journal. Missing prefixes,
incorrect identities/keys/MACs, complete bad excluded tails and torn final records
fail without repair. File replacement, unlink, append or size/mtime changes
observed during scanning also fail. On any error, cancellation or descriptor
close failure, the entire report is cleared. No directory/file is created,
truncated, synced or published. Cancellation is checked per record, not an
unconditional wall-time bound on filesystem IO.

The primitive grants no member-state authority, role selection, ready state,
native-store authentication, recovery, prune or promotion permission. Cursor
equality alone does not authenticate native bodies.

`InspectOrderingLedger` now adds the Usage-specific native binding: independently
authenticate the seeded Ledger baseline head and complete native history, read
each Ledger range through one finite native reader, reconstruct the exact frame
and compare its ordering digest. For each structural roll, fully authenticate the
native source, compare actual sealed segment metadata, and compare the roll frame
digest. Every ordered Ledger range/roll is checked, including excluded tails.
Other stores' native bodies are outside this helper. Historical roll lookup is
still a streaming scan with repeated authentication; its total IO can grow with
roll count times history and is not an indexed I-12 implementation.

Actual `halro usage verify` uses this binding and independently selects Primary
durable or Replica applied evidence, checks the durable term, builds fresh
Ledger/Usage references and revalidates member/Metadata/native anchors before
publication. See the [offline command contract](usage-offline-verification.md).
The durable business reference does not grant quorum confirmation or promotion.

The native files must remain quiescent under caller-owned locking. Descriptor
checks are not a replacement for that ownership. There is no history collection,
keyed snapshot reopening or repair fallback. Local regression evidence is in
[the original prerequisite record](../verification/performance/2026-10-03-usage-offline-prerequisites/README.md)
and [the native-bound command record](../verification/performance/2026-10-03-usage-offline-disk-verification/README.md).
