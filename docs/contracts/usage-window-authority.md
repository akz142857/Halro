# Usage window archive authority

Local implementation, 2026-10-03. This closes the raw-frontier entry point of
the [window primitive](usage-disk-window.md); it does not enable default Runtime
disk selection, date retention, native trim or authenticated saved-DB recovery.

## Issuance and binding

An existing unknown root requires a successful `VerifyDiskArchive` genesis
comparison to mint the Exporter's private window witness. The exact verifier checks archive fields, duplicate and
missing rows against the selected fresh owner's complete attempt interval. The
caller remains responsible for native authentication and role-allowed owner
construction. A schema/hash-only directory check or caller-supplied watermark
cannot mint this witness. An explicit `VerifyDiskArchiveAfter` scope cannot mint
it even if that narrower comparison succeeds.

A published retained-date root can now restore window authority only through
the complete fresh `VerifyPublishedDateCatalogStep` comparison, including its
independent native-key policy receipt and original prune baseline. Every row
through H is either matched or an authenticated allowed date omission. Candidate
Ready, activation alone and the general scoped date verifier grant no window
permission. See [startup/window reapplication](usage-startup-window.md).

The witness is fixed-size and ephemeral: selected owner generation, exact root
position/digest, captured manifest inode/size/mtime and directory inode. It holds
no descriptor, row history, native key or persisted trust record. Reopening an
Exporter or selecting another fresh owner does not inherit it. Issuance occurs
after actual verifier callbacks, scratch cleanup and file/DB closes return;
error, cancellation or owner retirement leaves no newly usable witness.

A missing root can obtain the same ephemeral authority from the publisher's
actual complete source prefix starting at sequence zero. The source rejects a
start before its removed-attempt cut; a missing catalog cannot invent deleted
source facts. For an existing root, publishing additional rows alone grants no
authority over its old prefix. It must match a previous native-relative witness.

`Exporter.PruneVerifiedWindowStep` accepts cutoff policy and finite limits,
without a sequence argument. It checks this witness and rereads the bounded root
against its exact captured directory, file identity and content before calling
the now-private row deletion primitive. A new native append may advance H;
only the previously verified archive frontier authorizes deletion, leaving new
unexported facts intact. Multiple steps can finish one unchanged root.

## Lifetime and invalidation

The subsequent [Exporter lifetime](usage-archive-lifetime.md) permanently
withdraws reader/writer admission on Close and joins their actual exit before
clearing both usable and pending window witnesses. Runtime/offline owners keep
native/Metadata dependencies and the directory lock alive through that join.

Lock order is Exporter, exclusive heavy/maintenance lifetime, then the short
physical projection write. Both admissions have zero queue. Root work and its
actual file close remain inside that physical lifetime; cancellation does not
release it while a callback is blocked, and projection Close joins actual exit.
Root/directory changes detected after a committed row step withdraw its report;
they cannot roll back an already committed derived transaction.

A new verifier operation withdraws the old witness before checking its scope;
failed or scoped verification also clears the private publication retry receipt.
Disk export withdraws usable authority while running. Only an exact matching
same-owner root and directory can supply the old-prefix proof. The source's
complete finite suffix and the publisher's constructed root bytes can then
extend it. The root receipt captures the actual temporary inode and the digest
of bytes written (streaming for legacy roots), rather than trusting a subsequently
read watermark. The root, page and partition temporaries report actual close
and ownership-checked unlink failures. Success is withheld until barriers, root
recheck and the complete physical job cleanup all return successfully.

Rename followed by barrier failure, cancellation or cleanup failure leaves at
most one private fixed-size pending root receipt. It is not usable by window
cleanup. A retry must match its exact owner, directory, root inode/size/mtime,
position and digest, then repeat file and directory sync before continuing.
Only successful physical completion promotes it. This allows finite publication
after earlier live rows have been trimmed, without a full genesis recheck on
the trimmed projection. Unknown, replaced, reopened or scoped roots do not
inherit this receipt. Fresh process recovery still requires a fresh complete
native reference and explicit genesis verification.

For a published retained-date root, the corresponding fresh published comparison
is required instead. An inherited native-relative publisher receipt can extend
that permission only with the same date predicate and original prune H. Newly
arrived old-date rows above that baseline must still be published.

The witness relies on the caller's exclusive data-directory ownership and
immutable already-verified data/catalog ancestors during this owner lifetime,
as the existing paged publisher does. Checking the root again does not rescan
every historical partition on each window step. Deep physical mutation is caught
by full verification/reopening, not converted into deletion-resistant D-03 trust.
The API does not authorize another process to mutate an archive under this owner.

## Verified scope and remaining work

Tests use authenticated native facts and real archive publication. They cover
unverified refusal, new unexported facts, another owner/Exporter, identical-byte
inode replacement, directory replacement preserving the manifest inode, forged
frontiers, cancellation, scoped verification, corrupt partitions and publication
invalidation of unknown publication. A blocked callback holds the actual manifest descriptor while
projection Close waits; after cancellation/exit that descriptor is closed and no
success report survives.

Publication tests cover both NDJSON and Parquet, both paged and preserved legacy
roots, finite one-row prefixes after actual trim, an independent full native
comparison of the final archive, post-rename barrier failure and exact retry,
replacement/reopening refusal, incomplete or trimmed genesis refusal, actual
stage-path replacement cleanup failure, and cancellation with Close joining
the actual rename callback. Publication source remains responsible for authentic
native construction and role authority; the receipt does not authenticate keys.

The default Runtime still uses Aggregate and its selected-disk window guard
remains. Date coverage and reclamation,
rebuild-before-ready window application, physical fees/capacity, HA publication
and trusted incremental recovery remain unfinished. Evidence and exact source
cut: [initial authority verification](../verification/performance/2026-10-03-usage-window-authority/README.md)
and [publication continuation](../verification/performance/2026-10-03-usage-window-publication/README.md).
