# Authenticated immutable snapshot reader

Status: implementation candidate, 2026-10-04. `internal/authenticatedstate` has
no Runtime caller. It does not authorize native resume or replace the complete
rebuild currently required on startup. D-03 is open; the production Seal,
identity/baseline publishers and incremental update protocol are prerequisites.

The caller supplies a complete, strictly ordered **authenticated** row stream
and an independently expected context digest. A digest of arbitrary live bbolt
rows is not an eligible source. A future domain publisher must establish the
source, complete bucket/schema scope, instance/member identity, native cut and
current role prefix. `Build` authenticates its supplied statement; it cannot
establish those facts on the caller's behalf. No wrapper signs the current live
Ledger DB. The real Ledger test proves representation/reading of a frozen,
freshly rebuilt fixture, not protection against modifying that ordinary source
DB while a future publisher is building.

The component writes one exclusive immutable pack. Its 160-byte `HLSTATE1`
commitment contains an expected context, random file identity, exact extent,
complete row count, tree height and root reference; HMAC-SHA256 covers its
canonical bytes with a distinct commitment domain. Unused bytes must be zero.
Empty roots have one context-specific representation. The root itself does not
contain a typed production schema/role/native cut; their validated encoding
belongs to the future Seal publisher. The key must be a separately derived
stable purpose key; the test's HKDF domain is expressly test-only.

Each leaf holds one complete key and a SHA256 blob reference. Ordered branch
references authenticate child bounds, counts, heights, offsets, lengths and
hashes. Each child must match its parent's exact description when visited.
Lengths, extent arithmetic, cardinality sums and height are checked before
allocation/recursion; bytes are hashed before decoding. An empty value has an
actual one-byte prefixed blob and is distinct from missing bytes. Physical
references contain offsets, not paths or normal DB lookup hints.

`Get` proves absence through authenticated bounds or a verified leaf.
`Floor` proves the greatest complete key <= target by descending the rightmost
eligible authenticated child. It rereads and verifies the selected path/value;
an altered or missing page never becomes absence. The caller checks the returned
scope because the preceding key can belong to another bucket or WorkUnit.
`Range` enumerates the tree itself; it never trusts an auxiliary cursor for
completeness. Bounds are inclusive/exclusive, with nil denoting infinity.
Continuation MACs bind the exact root header, range and last returned key;
malformed, noncanonical, other-root and other-range cursors are rejected.
Errors discard the complete output page, including its cursor. Missing or
changed referenced content is a sticky reader integrity fault. The enclosing
Runtime must eventually translate such a fault into its global readiness gate;
that integration is not implemented by this package.

Compiled codec ceilings are a 32,769-byte complete key, 3 MiB value, 512 KiB
node, four children and height 32. A query returns at most 64 rows/8 MiB and
reads at most 4,096 node/blob objects. These are decoding safety ceilings,
not approved D-04 defaults. The limits include full bbolt key width and the
existing Ledger projection value ceiling rather than the tiny original
prototype's 64-byte key/256-byte value fixture.

A caller-provided shared `resourcebudget.Manager` is mandatory. Before any
source read/file creation, build reserves 32 MiB logical workspace, 4 KiB
resident owner and the explicit maximum output bytes. It retires build RAM and
unused disk credit after durable completion. Reopened readers borrow persistent
bytes from their external publisher and retain a charged resident owner. Each
query reserves 32 MiB logical workspace; the handle has one active query with
zero waiting queue. Rejection, cancellation and byte/page quota failure do not
turn into successful empty results. These are conservative logical estimates;
Go GC, RSS, filesystem allocation/cache and all other runtime owners still
require physical capacity evidence.

Creation writes pages before its commitment, synchronizes the file, closes the
writer and synchronizes its parent directory before returning the pinned reader.
This is an exclusive candidate, not atomic replacement of an active Seal. The
reader checks the captured inode/path, exact size and unchanged header before
and after successful reads, while every accessed body is checked against its
reference. Untouched subtrees are committed, not newly audited. Close joins this
handle's real IO and clears its key; it does not refund owned persistent bytes.
Discard closes before deleting only the captured fresh inode. A replacement
survives and retained disk credit is not silently refunded. Reopened handles
cannot unlink anything. Cross-handle/root pins, interrupted orphan recovery and
physical disk refunds remain the enclosing publisher's responsibilities; a
caller must retire every borrowed reader before discarding the owner.

Build is a bounded full stream, with four child descriptors per level; incremental
updates now use the separate candidate [Store transaction and root publisher](authenticated-state-cow.md).
Whole-pack creation and a one-row leaf cost are explicit
tradeoffs of this first import/reader candidate, not a frozen production storage
layout. The Store candidate verifies old paths and atomically publishes a cut
digest with changed content; the production publisher still must establish typed
native/role eligibility, full scope/source authenticity, crash/GC ownership and
physical capacity. No arbitrary ordinary DB becomes an eligible signed source,
and no Runtime path uses this component to skip history yet.

Evidence: [reader and actual Ledger fixture](../verification/performance/2026-10-04-authenticated-state-reader/README.md).

The later [native Ledger candidate](authenticated-ledger-native.md) constructs
complete rows directly from authenticated facts through Store transactions,
without the ordinary source DB used in the earlier representation fixture.
It has no Runtime caller or reopen/resume authorization API.
