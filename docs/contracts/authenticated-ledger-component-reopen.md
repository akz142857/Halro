# Authenticated Ledger component reopen

`OpenAuthenticatedLedgerProjection` is a maintenance component API. It accepts
an independently established component Identity, saved ReplayAnchor, explicit
non-nil role-permitted Prefix and reconciled persistent DiskReservation from the
same Manager. These arguments do not themselves authorize a Runtime role or
business readiness. Production startup remains on fresh reconstruction.

Before component mutation, the factory authenticates the complete physical
native source, including compressed generations, physical empty-roll starts and
the excluded durable suffix. It proves both supplied cuts are actual boundaries,
including hash/epoch for the saved cut, and checks the full head against the
frozen opened Log. Checksum-era history remains ineligible without an independent
legacy baseline. A cut below the saved state refuses; it never lowers watermarks.
Role permission is supplied independently, never inferred from durable presence.

Core OpenStore checks the authenticated root against the exact identity/codec/
catalog/reducer scope and saved cut, the root-to-pack identity, persistent ceiling
ownership and fixed namespace lock. A typed authenticated point read verifies
that the root's position row equals the independently native-proven saved anchor.
Subsequent queries retain full value/path/absence/range authentication; unused
subtrees are not advertised as freshly audited.

Only the permitted delta is reduced, with the same atomic native-fact/root
transaction and canonical reducer as live Advance. The rest of the suffix is
authenticated as well. Typed readers are returned only after the entire factory
succeeds. Partial Apply failure returns no owner/receipt; persistent bytes remain
charged. A failed or mismatched active root is not silently rolled backward or
blessed with an old anchor. Recovery must independently select a valid current
cut, or rebuild a fresh generation from native facts.

The factory takes a native mutation ticket and the maintenance lease's exclusive
slot before IO, uses the native writer lock throughout and joins cleanup before
retiring the ticket. Lease/log Close cancels and waits for an accepted factory;
blocked IO retains its fees and exclusivity. Replica opens require no semantic
lease, use only the caller's explicit permitted cut and generate no native fact.

`AuthenticatedLedgerReopen` reports **SourceVerified**, **DeltaVerified** and
**AppliedRecords** separately. Full native Open/inspection still decodes history;
only semantic projection rebuild/write work is incremental. This is not evidence
that end-to-end startup IO/time is independent of history.

Borrowed OpenStore writers cannot delete the persistent namespace. Close refunds
resident/source RAM and borrowed references while publisher Disk stays charged.
An original closed publisher may `AdoptClosedSuccessor` only after the borrower
has closed successfully: exact Manager, namespace, directory/pack/lock identities,
scope/pack ID, non-regressing revision and captured root bytes are checked under
the namespace lock. This changes deletion receipts, not domain trust or disk fees.
Actual Discard reacquires the lock and preflights the entire three-file namespace
before removing its first file. A replaced/mutated root or extra file refuses
before pages deletion; disk credit is retired only after actual removal/sync.

After process exit, Go Permit values cannot be revived. A caller must reconcile
and acquire a new persistent owner. The cross-process test explicitly charges a
private namespace ceiling with a new Manager; no production Disk configuration
or filesystem-wide startup reconciliation is implied.

Still open: full identity/incarnation/publisher role-term Seal, cross-component
cut/roots, persisted root selection/publication, Metadata epoch baselines and
rotation, safe native resume tokens/Open for every log, historical audit deadlines,
Unknown/Pressured persistent startup admission, physical/aggregate resource
calibration, production Runtime wiring and acceptance. I-11 is in progress; M3
remains disabled. [Local evidence](../verification/performance/2026-10-04-authenticated-reopen/README.md)
records the real process, native, lifecycle, corruption and reference checks.
