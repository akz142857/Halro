# Authenticated state incremental update and component root publication

Status: candidate implementation, 2026-10-04. `internal/authenticatedstate.Store`
has no Runtime/production Ledger caller. This closes the component's incremental
read/modify/publish loop; it is not the full D-03 Seal or I-11 startup authority.
[Reader contract](authenticated-state-reader.md) remains the content/query basis.

`CreateStore` requires an already authenticated complete sorted stream and an
independently established scope/cut, as `Build` does. Nothing accepts the current
ordinary Ledger/Metadata DB automatically. Every later Transaction reads old
values and absence through authenticated paths, and mutations inherit verified
old references rather than re-signing data from an ordinary auxiliary DB.
Callbacks must derive domain mutations from already durable/eligible native
facts; this component neither verifies that provenance nor checks a HA role.

## Content and root layout

The store has one append-only `pages` file and one fixed `active.root`. The
initial pack's immutable 160-byte header binds its random file ID and stable
scope. Offset-only node/blob references remain in that pack; a root's committed
extent bounds every reference even as later revisions append outside its prefix.
The physical reader permits growth up to the independently configured ceiling,
verifies the pinned pack/header and checks every accessed object's hash. It does
not newly audit inherited unused subtrees on each update.

The canonical 240-byte `HLCOWR01` root contains an opaque independently eligible
native-cut digest, nonzero monotonic revision, the complete 160-byte content
commitment and an outer HMAC with a separate root domain. The content commitment
binds the stable scope, pack identity, committed extent, complete count, height
and authenticated root reference. A canonical empty content ref/height/count can
coexist with the pack's retained old pages. These candidate formats are not
frozen production Seal/schema/native-position encodings.

`OpenStore` requires an independently expected scope and cut, rejects another
pack even when the root has a valid MAC, and bounds the exact fixed root before
allocation. Revision/cut/commitment MACs do not establish current role eligibility,
latestness, a Metadata baseline or protection against whole-directory rollback.
A publisher must retain and replay later facts before serving an older root.

## Bounded transactions

`Update` is a zero-queue serialized callback with one private candidate root.
`Get`, `Floor`, `Range`, `Put` and `Delete` use the same candidate, so later operations see
earlier private mutations. Range cursors bind content, native cut and revision;
a cursor from a previous transaction cannot survive unchanged contents at a new
cut. Missing/corrupt referenced pages/values fail; replacement and deletion
validate the previous value before discarding it. Any ignored method error
aborts the entire batch. A callback panic retires its transaction and faults the
store while preserving the panic. Escaped transactions cannot perform later IO.
Close/return waits for actual active transaction method IO, not just cancellation.

A mutation clones a bounded input and rewrites only its verified search path.
Splits have at most five descriptors; short inherited children are padded to
preserve branch levels, and unary roots collapse through verified paths on
shrink. Complete key-set/range proofs use the resulting authenticated structure.
Neither full row enumeration nor ordinary DB cursor hints drive a normal update.
The implementation is bounded at the existing height ceiling; disk grows with
COW history until a future verified compaction/GC protocol retires old pages.

Ceilings per transaction: 64 mutations, 8 MiB cumulative key/value inputs,
256 read calls, 8 MiB cumulative returned bytes and 4,096 object reads, plus the
explicit configured maximum appended bytes and whole-pack ceiling. It reserves
64 MiB conservative logical RAM before invoking the callback. These are candidate
safety ceilings, not accepted throughput/physical capacity parameters. The
Ledger adapter still must prove its worst-case dependency/mutation footprint and
reserve accepted-work completion capacity before durable facts are written.

## Publication, errors and reopen

Pages/blobs are appended at the old committed extent. Successful publication
orders `pages written → pack Sync → exclusive root.pending write → root Sync
→ root Close → root rename → directory Sync`; physical Close errors are joined.
The complete new root becomes visible in memory only after the barriers and
caller context succeed. The prior active root's inode/full fixed bytes and the
pack/parent identities are checked; root staging replacement is preserved.
A receipt means this component publication completed, not native authentication,
HA confirmation, whole-component startup readiness or power-loss acceptance.

Failures after appending or beginning publication fault all snapshots and
further writer operations until reopen with independently eligible cut evidence.
A rename followed by failure can leave a complete new root on disk while the
operation returns no receipt. Reopen authenticates the allowed root, removes a
valid MAC-bound unpublished next-revision staging root and trims only the
uncommitted pack tail under its exclusive writer lock. Malformed/partial/foreign
staging is preserved and returns recovery-required; it is never deleted or
promoted based solely on its filename. Its controlled recovery/rebuild belongs
to the complete publisher protocol.

There are four fixed root-pin slots. A successful update leaves old pins reading
old referenced content, with root-specific cursors. Content integrity faults
propagate atomically to every pin. Store Close joins all active pins/writer work,
closes real descriptors and clears owned keys; concurrent Close calls join the
same retirement. Atomic pin retirement avoids a lock cycle when a callback
closes an independent old pin. The future Runtime must translate the store fault
into global business readiness=false.

## Persistent resource ownership

Creation initially reserves pack ceiling and bounded control-file bytes through
the shared manager, then merges retired same-class claims into one persistent
store owner. It covers the pages ceiling plus two root files and bounded lock
contents; filesystem blocks, directory/sibling-lock metadata and cache costs are
not physical acceptance evidence. A closed fresh writer retains this disk charge.

`DiskReservation` lends a retained reservation after writer Close. `OpenStore`
requires a live reservation in the **same** manager covering its configured disk
ceiling and atomically retains it for all writes/pins. Zero, wrong-manager,
undersized or revoked credits are rejected. `resourcebudget.RetainCovering`
checks lifetime/cost/manager and retains under one manager lock. The publisher
keeps its own reservation until persistent bytes are actually retired; reopened
stores release only borrowed references and their separate resident claim.

Fresh Discard joins its pins, obtains the real directory writer lock (so an
independent reopened writer excludes deletion), and removes only captured
pack/root/lock/directory identities. It never uses RemoveAll or unlinks foreign
replacements. Descriptor retirement and parent Sync precede release of its
persistent owner. External retained credits are not refunded by this deletion.
Reopened stores have no automatic deletion authority. Cross-process persistent
owner inheritance, root-inode adoption after restart, orphan budgets and complete
GC still need the publisher protocol; the subprocess fixture explicitly adopts
an independently verified successor inode for its private test cleanup only.

## Evidence and remaining integration

[Local evidence](../verification/performance/2026-10-04-authenticated-state-cow/README.md)
contains complete/old-root reads, map comparison, repeated shrink/split, ignored
errors/cancellation/quota/panic, every publication phase, six actual SIGKILLs,
reservation/Close/cleanup boundaries and history-size update read counts.
SIGKILL proves process interruption under this local filesystem/cache; it is
not a power cut, target Linux capacity or production HA/upgrade acceptance.

[The native Ledger candidate](authenticated-ledger-native.md) now shares the
complete domain hydration/reduction/index mutations with the existing backend,
starting at canonical genesis and reading actual native facts directly.
Authenticated Floor supplies latest cohort-version dependencies. This isolated
candidate eliminates ordinary source DB import, but grants no Runtime resume
authority and does not accept worst-case resource/throughput parameters.

Next: establish production first/source trust and typed all-component Seal;
implement authenticated Metadata baselines and every native incremental Open;
finish lifecycle/GC/full audit/terminal reserves and formal physical acceptance.
Default Runtime still selects full authenticated reconstruction.
