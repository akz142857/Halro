# Metadata native operation lifetime

The Store owns its bbolt handle, attached native journal and copied journal
key until native operations exit. This contract covers transaction entries,
queued/running batches, confirmation waits, journal attachment/application/
maintenance, native stats and compact snapshots. It is an ownership contract,
not a RAM/disk capacity reservation or a transaction-content authentication
protocol.

## Admission and shutdown

- `enter` admits an operation while the Store is open. Its reference ends on
  actual exit, including rollback after a recovered callback panic.
- Close permanently revokes admission, cancels the Store confirmation context,
  flushes accepted queued batches and joins admitted owners. Only then does it
  close the journal and database, release its journal reference and clear its
  copied journal key. Concurrent/repeated Close calls join the same outcome.
- An operation callback must return before Close can complete. Native
  transaction/migration callbacks must not synchronously call Close on their
  own Store. A confirmation callback receives cancellation but its actual
  return is still required; cancellation alone is not resource retirement.
- A batch entering just before revocation must either have joined the queue
  visible to the final flush, or return `bbolt.ErrDatabaseNotOpen`. It cannot
  enqueue behind that flush. A worker already running owns a separate reference.
- A committed authority withdrawal stays committed when its HA confirmation is
  canceled. The caller receives the confirmation error, which is not a durable
  rollback or evidence that the change was confirmed.

The Store's DB pointer and diagnostic path are fixed at construction. The
immutable path is necessary because bbolt clears its own path during Close.
The handle remains accessible internally for diagnostics/tests, but subsequent
Store entries refuse it; a successful native Close also makes raw bbolt reads
return `ErrDatabaseNotOpen`.

## Reads and journal maintenance

Captured read entries retain a revocable Store owner. Each invocation acquires
a reference through transaction exit; keeping a closure cannot reopen admission.
An Outcome-definition export releases its transaction between finite pages.
Its external callback may close the Store between pages, in which case further
reads fail. The view clears its retained read/context references on seal. The
Runtime export owner independently joins the entire response/publication scope;
Store read references do not replace that owner.

Journal attachment/trim/hook installation are exclusive relative to journal
operations and native commits. A commit captures the exact journal used for its
receipt before releasing that lock. Confirmation takes place after the bbolt
commit and outside the maintenance lock, so its callback can read Metadata.
The native log snapshots its confirmation callback under its own mutex before
invoking it. Close still joins that wait through the Store operation reference.
Runtime role/maintenance policy remains responsible for when journal trimming
is permitted and for confirmed-prefix authority.

## Cancellable native authentication

MetadataJournal Context variants preserve complete chain authentication. Reads
check cancellation before and after each actual Read of at most64KiB, and at
frame HMAC/decode boundaries. EOF versus torn-final-frame remains distinct;
cancellation returns no authenticated head and never authorizes tail repair
when authentication was interrupted. Already accepted filesystem work still
has to return; no goroutine closes a descriptor while Read is using it.

Standalone journal attachment, prepared-epoch authentication and replacement
verification use the admitted Store's closing context. Replica range application
combines caller cancellation and Store shutdown through its existing owned
confirmation context; complete validation still precedes independent commits.
Per-frame commits also check the Store context directly. Ordinary Replica
startup supplies its parent context to OpenReplicaContext. Legacy APIs keep
Background behavior; startup cancellation across all other native journals and
all migration steps remains separate work.

The scanner reuses one header, geometrically growing tail up to8MiB+32B and
HMAC/SHA state; the SHA chain hashes the exact header/tail without a second whole
frame copy. Decoded Record values remain independent and may be retained by the
caller. JSON/op allocations and caller-retained records remain separately
owned costs. Full-history validation still performs complete scans; this is not
an authenticated incremental cursor or a shared physical-RAM reservation.
Evidence: [scan cancellation and native compatibility](../verification/performance/2026-10-04-metadata-scan-cancel/README.md).

## Evidence and remaining scope

See [local verification](../verification/performance/2026-10-04-metadata-native-lifetime/README.md).
Tests block actual transactions and confirmation callbacks, prove the physical
database remains open until owner exit, preserve committed withdrawal on reopen,
exercise both sides of the final batch flush, retain closed read closures and
run real concurrent trim/commit/state reads.

Multi-step business operations, per-deployment pricing caches, whole-job
publication across stores, accepted-work capacity and physical bbolt/journal
fees still require their own owners/bounds. This change does not claim all
Metadata APIs or the complete memory-bounded-state design are finished. Full
repository gates, Linux capacity and production acceptance remain separate.
