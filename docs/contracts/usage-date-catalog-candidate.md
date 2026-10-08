# Usage date-retained catalog candidates

Local implementation, 2026-10-03. The finite candidate transformation and schema
10 consumers are implemented. Production root activation, independently verified
deletion authority and current-reference-safe file/page reclamation remain open.
This implements the construction stage of the [reclamation protocol](../architecture/usage-date-reclamation-protocol.zh-CN.md).

## Candidate construction and resume

PrepareRetainedDateCatalogStep accepts an explicit nonzero UTC date and a paged
baseline. Legacy JSON first needs the existing catalog migration. Each call has
1–16 input pages, 320 KiB–8 MiB work and at most a minute; defaults are four pages,
1 MiB and ten seconds. Input bytes and actual encoded output/checkpoint writes
consume the work cap. Before entering a page, the step reserves its input, at
most two output pages and a checkpoint. A lower byte cap stops between pages and
saves progress; it does not repeatedly discard the same unfinished batch. One input page and the bounded builder are retained at a
time; no historical date map, descriptor array or native row set is introduced.

A single checkpoint, .archive-date-build-v1.json (at most 8 KiB), stores the exact
baseline position/digest, caller date, next immutable input page, output head and
scanned count. Root/policy changes refuse the checkpoint rather than silently
choosing a new source. Active export remains possible, but an append changes
this candidate's frozen baseline and requires an explicit reset/new candidate.

The in-memory replay cache is fixed-size and holds no descriptor or row history.
Reopening loses it. Persisted progress is untrusted: successive finite calls
reconstruct its saved prefix from the still-matching baseline, repeat page/hash
and descriptor/date validation, and compare the exact deterministic output head
and input position before extending it. Changed checkpoint bytes also withdraw
the same process's cache. SHA identifies bytes; it is not saved-state authority.

Candidate pages use the existing canonical content-addressed catalog encoding.
Already present output pages must match and repeat durability barriers; failed
steps leave retryable orphans. The checkpoint is file-synced, actually closed,
ownership-checked, atomically renamed and directory-synced. A rename/barrier or
late cancel/close failure withdraws the receipt and replay cache; reopening can
revalidate the saved state. The writer mutex covers the entire physical job and
Exporter.Close joins actual exit. A logical cancellation does not release it.

ResetRetainedDateCatalogBuild explicitly removes only the reserved checkpoint,
under the writer lifetime and file identity checks; it refuses symlinks. It does
not remove live data or immutable candidate pages, change the live root/epoch,
or happen automatically on an error. Orphan cleanup is separate work.

The receipt exposes phase, cumulative scan/replay counts, pages processed in this
call, candidate head and bounded root JSON only at Ready. Ready means descriptor
transformation reached EOF. It does not prove native correspondence, authenticate
the caller's date policy or authorize root publication/window/native deletion.
The API never activates its JSON or unlinks live pages/data. Its baseline and
checkpoint checks do not substitute for final independent native/role evidence.

## Schema 10 and consumers

The candidate JSON uses schema 10 with the original processed H and a mandatory
retention object: version 1, retain_from_date and prune_through_sequence. The last
field is the frozen sweep baseline; a listed old-date file at/below that baseline
is structurally invalid. Later old-date attempts may be appended above it and
remain exactly verifiable until the next explicitly authorized sweep.

Schema 10 requires complete_prefix=false. It describes date coverage rather
than a physically complete historical row set. Older schema 9 readers reject its
schema; legacy readers cannot treat it as an empty files array. Missing, duplicate,
unknown/null retention fields, invalid date/version/range and retention metadata
smuggled into an older schema are refused. A root does not supply verification
policy: ordinary genesis/explicit sequence verification remains unchanged.

Current catalog reads, checksum jobs and exact native verification check the
scope against listed descriptors. Finite append preserves schema/date/baseline,
and a no-op repeats barriers without rewriting the root. Even a schema 10 archive
containing every native row cannot mint the all-history window witness. Normal
window/native deletion entry points remain separate.

Archive continuations are now version 2, carrying the captured retention scope
with H/page/index and cleanup epoch under the ephemeral MAC. Old versions fail
explicitly. Returned descriptors still do not pin later external file opens.
Default Aggregate Runtime still refuses paged/date roots, including repeated
failed opens; actual offline verification accepts only the caller's explicit scope.

## Remaining obligations

The subsequent [finite candidate row-set comparison](usage-date-catalog-verification.md)
adds per-call archive/source steps, a fixed-state disk radix index and untrusted
checkpoint-prefix replay. It compares against the selected fresh projection and
does not provide native/role/Metadata authentication or activation permission.
The subsequent [Standalone logical activation](usage-date-catalog-activation.md)
adds independent App authority checks, a single-use comparison proof and a durable
old-root obligation. Current-reference-safe bounded unlink/resume, CLI/default
Runtime/HA activation still need implementation. Existing native-construction,
whole-job exact-verification and startup-directory limits remain separate gaps.
Candidate construction can replay its saved prefix after restart; it does not
grant trusted restart or avoid all historical IO. Saved progress and output pages
consume physical disk; shared fees, orphan space, Linux capacity and deployment
acceptance remain separate, unverified obligations. I-07/I-08 stay ongoing.

Evidence: [candidate verification](../verification/performance/2026-10-03-usage-date-candidate/README.md).
