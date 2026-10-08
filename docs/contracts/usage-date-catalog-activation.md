# Usage date catalog activation and barrier recovery

Local implementation, 2026-10-03. The Go API publishes logical date coverage
and one durable old-root obligation. Physical reclamation, CLI exposure, default
Usage Runtime migration, HA activation and production acceptance remain open.

The original policy decision now requires a [native-bound authenticated receipt](usage-date-policy-authority.md). Unsigned legacy obligations cannot acquire deletion authority on reopen. This supplements the fresh row comparison and independent caller checks below.

## Independent authority and single-use comparison

`app.ActivateUsageDateRetention` accepts an explicit UTC date on an offline
Standalone directory. It refuses configured HA and persisted member state even
after configuration downgrade. The existing exclusive directory lock, keys and
fresh native-derived Ledger/Usage owners remain held through physical cleanup.
Native authentication and historical Metadata checkpoint checks precede finite
construction/comparison; final source/checkpoint/Metadata/member checks and actual
readonly Metadata Close precede archive publication.

After publication, App checks Metadata inode/size/mtime, reopens readonly Metadata
to compare checkpoint/applied position, closes it, and checks the full native
source and member guard again. Source changes, cancellation, retirement, actual
Close or cleanup errors withdraw the entire returned report. A committed root
remains with its obligation on late failure; it is not described as rolled back.

`Exporter.ActivateRetainedDateCatalog` consumes one private process proof from a
full candidate/current-root comparison on this Exporter and fresh projection
generation. It checks source head/window, root inode/size/mtime/digest/position,
build/verification checkpoint digests, directory identity and explicit date.
Saved Ready alone supplies no proof. The lower-level Usage API depends on the
caller's independent native authority; App supplies the integration above.

The proof is fixed-size process state, with no historical rows/descriptors/open
files. An accepted activation consumes it even on error; Busy admission does not.
Verification/reset/Close withdraw it. Reopen and repeated recovery require a new
complete comparison. No D-03, all-history/window or native trim trust is granted.

## Publication and obligation

Writer/reclamation gates admit without queuing. Active readers make activation
Busy. Actual publication, all file closes and checkpoint writes remain inside
the projection's heavy-job lifetime. Both owner Close calls join a blocked
publication; logical cancellation cannot shorten their physical ownership.

One canonical `.archive-date-activation-v1.json`, at most 128 KiB, preserves exact
bounded old-root bytes and canonical new-root bytes. Version 2 carries the
authenticated policy receipt; the derived `intent`/`published` phase remains a
recovery hint. Root fields and phase alone grant no native/deletion authority. Both roots
share H; the new schema-10 prune baseline equals H and `complete_prefix=false`.
A different pending root pair cannot replace the singleton obligation. There is
no historical deletion array.

Intent is file-synced, actually closed, ownership-checked, renamed and directory-
synced before changing coverage. Immediately before root rename, the operation
rechecks the saved obligation, candidate/checkpoint owners, source and directory.
It advances the cursor epoch before publishing through the existing file
Sync/Close/ownership/atomic-rename/directory-Sync publisher. Then it durably
publishes the obligation phase. Failure can conservatively invalidate cursors.
The report always has `ReclamationPending=true`: no partition/page/native file
is unlinked and no physical disk or unconfirmed fee is returned.

## Reopen and appended-root recovery

`VerifyPublishedDateCatalogStep` uses the finite file/source limits and fresh
fixed-node private index from candidate verification. It compares all listed
rows and the entire required set against `(0,current H]`: dates at/after the
explicit policy plus every attempt above the original prune baseline. Late
old-date facts above that baseline cannot be counted as expired omissions.
The intended candidate head must actually occur in the immutable current page
chain; the untrusted obligation cannot assert ancestry.

Separate 8 KiB `.archive-date-published-verify-v1.json` binds frozen current root,
source, ancestor and old/new root pair. Reopen replays saved comparison before
extending it. Root/source/policy changes refuse stale progress. Explicit
`ResetPublishedDateCatalogVerification` removes only that checkpoint and this
process's owned scratch. The saved root-pair digest excludes the derived phase
so finishing intent -> published does not invalidate recovery's own checkpoint;
the single-use process proof additionally binds exact observed obligation bytes.

Recovery repeats actual current-root file/directory barriers and publishes the
obligation phase. It preserves an appended root byte-for-byte rather than replacing
it with the original candidate. Saved phase/existence grants no permission without
fresh full comparison and independent App authentication.

## Remaining work

The App reference/authentication/construction job still has a total one-minute
deadline. Date steps do not make the whole workflow resumable for arbitrary
native histories. Oversized legacy partitions, complete data/page reclamation,
obligation completion, orphan cleanup, shared physical fees, CLI/default
Runtime/HA migration and Linux/production acceptance remain unfinished. Default
Aggregate startup still refuses date roots; this API does not switch that owner.
[Finite current-root reference construction](usage-date-catalog-references.md) now
provides bounded data/page named-path membership tied to the physical current root.
[Single-file](usage-date-file-reclamation.md) and [finite-step data reclamation](usage-date-data-reclamation.md) now combine those references with exact native/file/parent checks. The [policy receipt](usage-date-policy-authority.md) independently binds the original decision; page/orphan cleanup and obligation completion remain pending.
I-07/I-08 remain ongoing. See [evidence](../verification/performance/2026-10-03-usage-date-activation/README.md).

[Finite original-page reclamation](usage-date-page-reclamation.md) now follows an
actual completed data job, with signed next-hop intents that survive deleted old
heads. It retains this signed activation and the old-root obligation. Orphan cleanup,
obligation completion and subsequent policy activation remain unfinished.

[Explicit orphan data-file inspection](usage-date-orphan-file-reclamation.md)
now derives a bounded candidate from actual bytes, then independently verifies
native content before unlink. It works after old pages disappear and keeps the
original receipt. Directory inventory, orphan pages and safe next-policy handoff
still have no completion claim.
