# Usage public queries and selected disk view

Status: default Standalone disk selection implemented, 2026-10-04. I-07 remains
ongoing. Primary also selects role-bound disk Usage; Replica retains its
authenticated applied-prefix legacy owner. Default public queries
use the [fresh Standalone startup owner](standalone-usage-runtime-owner.md) after
native recovery, archive validation and stored-window reapplication. Saved Usage
DB/checkpoint bytes never supply restart authority or a fallback.

## Selected view and public results

The Runtime Usage subsystem groups its explicit legacy Aggregate, selected disk
query alias and response lifetime. Attempts, failed requests and request detail
call that selected view. Summary, Dashboard, onboarding and system status
also select that owner when present, as described below. A disk error cannot select Aggregate, manufacture an
empty success or turn a damaged owner into a missing request. An unavailable,
busy, canceled or source-budget-limited disk query returns 503. Invalid query
parameters or cursor return 400; an actual absent detail returns 404.

Each public query acquires one zero-queue response ticket before CatchUp,
parsing, query preparation or response encoding. It holds the ticket through
the actual HTTP Write. The operation context has a one-minute deadline, raw
query text is at most 1 MiB and the success JSON response is at most 8 MiB,
including its final newline. These bounds do not charge the shared RAM Manager;
encoding and retained response bytes still need physical fee calibration.

Business drain, listener failure and direct terminal Close permanently withdraw
fresh query admission. Business drain joins admitted query responses before
background/accounting retirement. Direct Close joins them before closing native
and Metadata dependencies, even if no HTTP Shutdown proof exists. A blocked
Write can extend the real join beyond its request deadline; cancellation is not
evidence that the actual callback or its owned bytes have returned.

## Attempt and failure continuation

One HTTP call reads one finite physical page (160 source rows, 8 MiB input and
8 MiB output, at most 100 result rows). Sparse predicates and an unfinished
request-to-attempt failure join may produce an empty page with a nonempty
`next_cursor`. Such a page is not an end-of-results claim. Console attempt and
failure panels keep their pagination control until the cursor is empty, rather
than displaying a terminal empty state while older matches remain unread.

Tokens use version 2 and fixed 196-character raw URL-base64 values. Their fixed binary payload
contains version/kind, owner generation, query digest, frozen native position,
scan position, window-cleanup epoch and the bounded failure-join decision. HMAC-SHA256 uses a fresh
32-byte random key independent of the public generation. Neither the key nor
the tokens' server-side state is persisted. No unbounded token-to-state map is
introduced. Forged positions, changed filters/limit, another endpoint or a
fresh owner after restart cannot continue the old token. A bare legacy sequence
cursor is not accepted by disk public queries. Actual [window deletion](usage-disk-window.md) advances an atomic epoch and rejects older continuations; normal appends retain their frozen-prefix semantics.

The cursor's frozen prefix excludes later appended facts, including matching
ones. Failure continuation retains its pending candidate and latest-attempt
position, so an earlier matching attempt can establish the predicate without
replacing the true latest failure context. This is a query capability for an
already authenticated admin, not authority for native facts, role eligibility,
saved content or a mutable historical DB snapshot.

## Complete request detail

The existing response remains a complete summary and attempt chain in native
arrival order. The disk implementation pins an immutable summary version and
native head across short page transactions. The entire job shares the
zero-queue heavy-query slot with disk Summary and holds its lifetime lock until
all pages, validation and preparation finish. Physical Close joins this job
even between its source page transactions.

The public detail budget is 128 pages, 4096 attempts, 64 MiB input, 8 MiB output
and one minute. The output-work budget conservatively charges repeated summary
copies across pages as well as the final encoded response. Exhaustion, missing
companions, mixed cuts, cancellation or retirement returns no partial detail
success. Missing requests return 404; exceeding a limit returns unavailable.

## Complete disk Summary

Public Summary selects the same disk query owner and its frozen rollup prefix.
It does not merge old Metadata rollup rows or borrow Aggregate. Existing
validation, day/month/year buckets, five grouping dimensions, filters, sorting,
labels and optional response fields retain their wire behavior. A disk error
returns 503 without partial success or legacy fallback.

The job uses one exclusive `dataDir/usage-summary-scratch` directory. Before
creation, both that namespace and `summary.db` pass the configured/computed
native authority and physical identity checks. Existing paths are preserved
and refuse the query. Cleanup removes only the job's own identities before the
HTTP response is admitted for writing; the response ticket still joins the
actual Write. A crash leftover can keep future summaries unavailable until
explicit authority-safe startup reconciliation is implemented.

Explicit ceilings are 4096 pages, 65536 source rows, 64 MiB source bytes,
8 MiB output, 1048576 scratch reads, 256 MiB scratch read bytes, 256 MiB scratch
write bytes, 64 MiB scratch file, 4096 periods, 8 MiB period work and one minute.
Per-page limits remain 160 source rows and 8 MiB input/output. These logical
work limits do not reserve physical space or charge shared RAM/disk fees.

## Dashboard, onboarding, status and metrics

Dashboard uses the selected disk owner for totals, quality, groups and hours.
The first-value flag uses global request counters from the same admitted read
cut, including when the current day is empty. System status reads only the
selected owner's errorful position; its legacy adapter does not enumerate
checkpoint segments. These handlers and onboarding hold the public response
ticket through actual HTTP Write, with no Aggregate fallback after disk errors.

Onboarding requires a successful finalized request joined to a successful
attempt. Its proof is the maximum CompletedAt for each current Provider,
Deployment or Route, not the last arrival and not a global success flag.
Existing configuration UpdatedAt/revision and proxy-runtime checks still
invalidate old traffic proof. The latest finalized request and its latest
attempt context remain in native arrival order, including late settlement.
The disk job scans short transactions under one admitted coherent read cut;
its maps contain only current targets, with no historical request-ID set.
Malformed or missing terminal associations fail; an immutable final version
cannot become a pending request just because an index key disappeared.
Full key-set deletion detection remains part of D-03.

Public onboarding caps target preparation at 4096 current configuration entries
and 1 MiB ID text. The whole disk job caps 65536 charged rows/joins, 64 MiB input,
8 MiB output and one minute; individual transactions scan at most 160 attempts.
Exhaustion returns 503 with no partial readiness. Scanning beyond those limits
needs further index/capacity work. Metadata configuration list APIs still
materialize their own lists; this query migration does not bound those lists.

Metrics read constant disk state and committed page extent in one transaction.
They omit resident window/checkpoint families for disk owners and omit all
Usage-dependent values if the selected read fails, while exporting explicit
unavailability and preserving other diagnostic families. Page extent is not
physical allocation or RSS. Metrics have their own configured 1–32 response
slots, remain available during business drain, and are permanently withdrawn
on listener failure/terminal Close. Close joins actual writes before native
and Metadata dependencies close. See [metric definitions](metrics-reference.md).

## Outstanding migration work

Standalone Collector, archive export, recurring date retention, window trim and
all public observers now select the same disk owner. Member allowed-prefix
publication remains separate unfinished work. Test-only finite materialization
reads actual selected disk pages/counters; it cannot enable a resident production
fallback. Legacy checkpoint-lock tests explicitly select the legacy adapter.

Shared physical RAM/disk fees, mmap/freelist/dirty-page cost, trusted incremental
restart, retention cursor pins, batch archive/manifest publication, Linux
capacity, migration/backup and production acceptance remain unproven. This
query layer does not close I-07 or the overall memory-bounded goal.
