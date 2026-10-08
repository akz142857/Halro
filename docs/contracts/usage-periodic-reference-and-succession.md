# Usage recurring-retention prerequisites

Local implementation, 2026-10-04. Ordinary Standalone and Primary Open select fresh disk
Usage and their periodic ticks run the controller described below. I-07/I-08 remain
ongoing; local integration evidence does not establish production acceptance.

## Full-history maintenance reference

`Runtime.withUsageMaintenanceReference` admits one operation with no waiting
queue on the selected Usage owner. Standalone uses its native owner; Primary
requires its authenticated member identity and unchanged role/term/promised-term.
Replica, retired/mismatched owners and caller cancellation are refused.
It uses a one-minute context and owns one private temporary directory outside
native data, containing fresh Ledger and Usage disk projections.

`Log.BuildFreshProjection` authenticates actual native history before creating
the Ledger reference and holds the real native writer lock through construction.
The selected native head/hash must equal the physical authenticated report.
This Standalone builder refuses replicated hooks/Replica mode. Primary instead
builds a fresh private disk reference from a fixed native cursor, with full
authentication and member checks before/after archive work. Closed/unhealthy
native owners are refused; native
integrity failure latches recovery. Existing derived/checkpoint bytes supply
neither rows nor authority. A new empty Ledger is an explicitly tested path.

Usage then replays authenticated native facts through that private Ledger cut.
An appended/rolled suffix is authenticated but excluded. The committed Usage
anchor's sequence/hash must equal the private native cut. Live query rows may
already have been trimmed and never supply the historical reference. A final
physical prefix check and authenticated native replay precede actual derivative,
DB and scratch cleanup. Cancellation after cleanup still withdraws success.

The physical scans temporarily delay native appends/rolls. They do not acquire
semantic maintenance ownership, retire the business Manager or authorize an HA
durable tail as applied business state. Complete scans and reconstruction remain
O(history); the one-minute deadline is a refusal boundary, not capacity evidence.

Runtime close cancels this owner, rejects further jobs and waits for the actual
callback, native operations and private cleanup to finish before closing the
exporter/native source or clearing the independently owned native key. A second
operation returns Busy; it cannot create another historical owner/queue.

## Fixed-slot date-policy succession

The signed activation intent may now contain one `PreviousCompletion` digest.
Its receipt MAC binds that digest, exact old/new root bytes and native prefix
under `halro/usage/date-policy/receipt/v2`. Records without this field preserve
the prior canonical bytes and v1 receipt MAC domain. Old readers that cannot
decode the new field must refuse the new record; release compatibility remains
part of the migration gate.

A different current marker may be replaced only after the actual independently
authenticated active completion matches the published current retention and
original prune H. Its policy binding must match the replaced marker; a different
future intent remains Busy. Missing markers also require that completed current
policy when the root already has retention. The exact completion bytes and inode
are checked and synced before intent publication and checked again at root rename.
The active completion remains present while the new intent/root is published.

The new signed-page state can replace the previous completed state only when
the successor receipt binds the actual active completion, the old page state
equals its authenticated original-chain result, and a genuine successful new
data job supplied this owner/root/native/window's private completion witness.
Previous completion alone and saved Ready cannot bootstrap new page deletion.

After the successor's own original page chain completes, its active record may
atomically replace the previous active record using the same authenticated
link. Each record retains only a digest, not a prior record/list, so successive
policies do not enlarge the control state. Late old-date native rows above the
prior H are still published; only a later fresh sweep advances the signed prune
baseline and may omit them. Reopening must authenticate the real source again.

`OriginalChainComplete` still means the named original chain was processed.
OrphansPending remains true; no global orphan absence, physical fee refund,
native prune or saved-projection restart authority follows from succession.

## Recurring Runtime controller

`maintainUsageDiskCycle` catches up the selected Collector, runs dates before
appending this tick's archive prefix, then performs one finite export and window
trim. This ordering preserves an unpublished signed intent's exact old root.
A date failure suppresses append/trim and withdraws the in-process window proof.
Retention disabled in config cannot abandon an already published obligation or
pending signed policy; the controller completes those without starting a new one.

`ReadPendingDatePolicy` authenticates the saved receipt and actual old root.
The controller first completes an existing published obligation. At most two
policies run per job: an interrupted unpublished intent and the desired UTC
retention date. Derived build/comparison/reference/data hints are reset for the
new full-history reference; signed irreversible page progress is preserved.
`HasCurrentDatePageReclamation` authenticates and matches the current policy,
accepting an older completed slot only through the signed successor link.

`BindVerifiedWindow` requires a fresh full-history proof, actual native anchors
for both the reference and live query DB, and the unchanged root bytes/inode.
It rebinds only the fixed process-local proof to the live owner. Failure, owner
retirement or late native/cleanup failure withdraws that permission. The private
reference may close after a successful bind; no disk/FD alias remains in it.

Full-history reference construction and native prefix checks still have
O(history) IO. Shared physical fees, bounded restart IO, HA, migration/backup,
Linux capacity and production acceptance remain separate requirements.

See [current integration evidence](../verification/performance/2026-10-04-usage-default-runtime/README.md)
and [startup window](usage-startup-window.md).
