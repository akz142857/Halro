# Offline Usage summary rebuild

The actual `halro usage rebuild-summary` command now builds fresh disk Ledger and
Usage references from authenticated native history. It does not construct an
Aggregate, Snapshot, full rollup map, historical day set or checkpoint segment
list. The four existing JSON report fields are preserved.

## Authority and lifetime

Only an offline Standalone directory is accepted. Configured members and a
member directory with replication removed from configuration are refused before
writes. The command owns the existing exclusive data-directory lock. Metadata
schema/key/vault, the complete native chain, historical deletion anchor and
physical positions are checked independently before building the reference.

The shared [offline reference](usage-offline-verification.md) revalidates source,
Metadata transaction and named inode before writable publication. Its readonly
Metadata handle actually closes before the exact-schema writable open. That open
checks the authenticated inode, size and modification time on the actual opened
file descriptor, refuses creation/migration, and checks the original Metadata
position and Ledger anchor before writes. Publication does not attach a journal
or permit authoritative updates. Final native authentication, unchanged Ledger
anchor and Metadata epoch/sequence, named inode, context and physical cleanup
checks precede any success report. Own derivative commits advance Metadata's
physical transaction ID; they do not advance its journal authority.

The cooperative one-minute operation owns both disk references and the directory
lock through physical close/join. Any failure, cancellation or cleanup error
withdraws the report. A completed root may remain published after a later error;
retry rebuilds from native facts and never adds the prior result to itself.

## Finite source and publication

`RollupSnapshotPage` enumerates all six dimensions and every legal period label,
including labels outside the HTTP date-query range. A generation/head-bound local
cursor refuses changed owners or Apply between pages. Each page uses a short
transaction, at most 100 rows and 8 MiB input/output; no transaction crosses a
Metadata write. A transactionally updated scalar row cardinality makes a deleted
row set or missing remainder fail instead of becoming empty/complete success.
Rows validate identity, hash key, structure version and histogram semantics.

Metadata has two fixed lazy node-derived buckets, `usage_daily_rollup_a` and
`usage_daily_rollup_b`. `UsageRollupState.slot` selects zero (historical bucket),
one or two. A rebuild clears the unselected slot in transactions of at most 100
keys and 1 MiB key bytes, then writes finite source pages there. A zero-queue job
slot remains held until the source callback physically returns. Root witnesses
are checked in every transaction; duplicate keys, invalid identities, empty
nonterminal pages and source errors abort publication. Existing corrupt
derivatives can be replaced without using their contents as authority.

Whole-job limits are 65,536 cleanup steps, 65,536 source pages, one million rows
and 256 MiB encoded row/key writes, under the operation context. They are local
bounds, not calibrated production capacity. A limit failure leaves the old root
selected; a later invocation clears the same fixed staging slot in finite batches.

After the complete source ends, one small transaction publishes the new slot and
its exact semantic watermark together with an explicitly unrestorable checkpoint
marker. Accounting-day count uses the unique total row per period/timezone, not a
historical set. Normal incremental checkpoint writes and rollup range reads use
the selected slot; resetting derivatives withdraws selection and returns to the
historical path. Two slots bound generation count; inactive/legacy data and old
checkpoint segments are not wholesale deleted during this publication.

The generic Metadata recorder continues classifying and checking local writes,
but no longer clones an overwritten/deleted node-derived or node-local payload
that it would discard. Replicated and unknown classifications retain their owned
previous-value copy and confirmation behavior. Actual committed overwrite,
Delete and Cursor.Delete of an 8 MiB old derivative have a focused allocation
regression; this is not an RSS/freelist acceptance.

## Restart and remaining work

The marker uses unsupported checkpoint version zero and reducer
`disk-rollup-rebuild-v1`. It contains no restorable Aggregate and is not a D-03
content/key-set authentication root. Current Aggregate startup rejects it,
withdraws the slot and rebuilds both views from native WAL, so old stored rows are
not added to a full replay. This intentionally retains the full cold-replay cost.
An older checkpoint writer omits `slot` when replacing the state, returning to its
historical bucket; no stale separate pointer can survive that replacement.

Default Runtime Usage remains Aggregate. Window/retention and actual `prune`,
resumable history verification/migration/reclamation, shared physical fees,
bbolt mmap/freelist and disk growth, HA owner selection, authenticated snapshots,
Linux capacity and release/production acceptance remain unfinished. This is a
CLI maintenance implementation, not approval of the same-DB runtime layout.

Source-cut and gate evidence: [verification record](../verification/performance/2026-10-03-usage-summary-disk-rebuild/README.md).
