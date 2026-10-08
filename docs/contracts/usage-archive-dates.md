# Usage archive UTC date coverage

Local implementation, 2026-10-03. This adds native-relative date verification
to the [exact archive verifier](usage-storage.md). It does not implement physical
date retention, default Runtime disk selection, native trim or saved-DB trust.

## Partition dates

Exact verification now requires a canonical `YYYY-MM-DD` descriptor date in
years 1–9999, a canonical slash-separated path inside `date=<date>/`, and every
decoded row's UTC completion date equal to that descriptor. Both legacy and
paged roots and both Parquet and NDJSON use the same checks. Native comparison
still verifies schema-narrowed row fields independently. A valid row checksum
and correct native fields cannot disguise a forged expired partition date.

The finite publisher rejects mixed-day source rows or invalid partition dates
before creating its partition file. It groups by actual native completion dates;
the guard makes that grouping an explicit publication invariant.

## Explicit coverage policy

`VerifyDiskArchiveRetainedDates(ctx, projection, retainFrom, limits)` normalizes
the caller's explicit nonzero date to its UTC calendar day. It always traverses
the entire native interval `(0, archive H]`. Every native attempt whose UTC
completion date is on or after that date must be present and exactly match.
Older missing attempts are counted as authorized expired omissions. Every
archive row that is still listed, including expired rows, must match native;
unknown/duplicate rows and bad content never become permissible by being old.

The predicate is independent of arrival sequence. An old/new/late-old sequence
may omit both old portions while continuing to require the middle new portion.
Neither the first remaining row nor an empty directory establishes a floor or
cutoff. Claimed policy fields inside the root do not select the caller's policy.
The existing sequence verifier retains its explicit `(after, H]` semantics.

The scoped report includes the UTC cutoff, archive frontier, actual source head,
all native `SourceRows`, `RequiredRows`, `MatchedRows`, and `ExpiredOmittedRows`.
Matched rows must equal all listed archive rows. Even an all-expired empty archive
requires complete native traversal and an explicit policy; a missing manifest
continues to fail. Counts and reports are cleared on errors, limits, cancellation,
retirement, root changes or actual scratch/reader cleanup failures.

Date verification withdraws both the usable window proof and its pending
publication receipt. Even a complete archive checked with a date scope does not
mint full-history window authority. A report is not permission to prune files,
reuse an unauthenticated projection, truncate native facts or promote a member.

## Offline command and authority

```bash
halro usage verify --config ./config.yaml --retain-from 2026-10-05
```

`--retain-from` is exclusive to `verify`. It cannot combine with
`--after-sequence`, even explicit zero. Invalid/noncanonical/zero dates fail
before reading configuration or opening data. Omitting the flag preserves the
existing default genesis or explicit sequence comparison.

The command uses the same exclusive existing-directory lock and independently
authenticated fresh Ledger/Usage references as ordinary offline verification.
Standalone uses authenticated native history; Primary is bounded by signed
durable ordering, Replica by applied ordering and Metadata agreement. Final
native/member/Metadata evidence and all actual cleanup still precede report
publication. Native data, existing derivatives, member state and root bytes are
unchanged. Existing files retain identity/content/mode/mtime; reading may update
access time and temporary scratch creation may update directory mtime.

The CLI exposes `retain_from_date`, `required_rows`, `matched_rows`, and
`expired_omitted_rows` with the existing native/allowed/source/ordering scope.
`ledger_records` counts all compared native attempts up to the archive frontier;
it can exceed `parquet_records` under an explicit date policy. New fields are
omitted when using the old sequence interface.

## Bounds and remaining work

All existing source page/job, row, file, read, decoder/spool and physical index
limits apply. Omitted expired rows still consume source work; listed expired
files still consume archive decode work. No historical date map or retained-row
array is introduced. Catalog and orphan readers now also propagate actual Close
errors after physical exit instead of silently discarding them.

This remains a bounded whole-job verifier with full-prefix IO. Persistent resume,
date-retention root format and atomic replacement, bounded unlink
and orphan obligations, physical fees and Linux capacity are separate work.
Evidence: [date verification](../verification/performance/2026-10-03-usage-archive-dates/README.md).

The subsequent [archive lifetime](usage-archive-lifetime.md) adds actual reader
join, finite admission and epoch-bound continuations. It supplies exclusion and
invalidation, without implementing the remaining date root/unlink protocol.

[Date-retained candidates](usage-date-catalog-candidate.md) now implement finite
descriptor transformation, checkpoint revalidation after reopen, schema 10
coverage fields and policy-preserving append/consumers. Ready is not native
verification or publication permission; activation and bounded safe unlink remain
unfinished, as does resumable full-history exact verification.

The next [finite candidate comparison](usage-date-catalog-verification.md) supplies
per-call archive/source progress and saved-prefix revalidation against a selected
fresh projection. Native authentication/construction and final activation authority,
oversized legacy-file repack, safe unlink and default Runtime remain open.
