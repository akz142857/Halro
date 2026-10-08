# Live authenticated Ledger accounting reads

Status: component candidate, 2026-10-04. Extends [point reads](authenticated-ledger-point-read.md) on fresh complete-MAC native construction only. No ordinary DB import, saved-root Open, global Seal or Runtime/backend selection is added. The native reducer, key catalog and codec version are unchanged.

## Interfaces and one coherent cut

The candidate implements `GovernanceCapacityReader`, `AccountingWorkUnitReader` and `AccountingObservationReader`. Every whole query pins one authenticated root and reads required native position plus index/source dependencies in that root. One owner read lock remains held until physical pin cleanup and fee release; concurrent Advance fails Busy, never exposing partially changed counts or details.

Open WorkUnit capacity walks only the selected project's open index, checking each real WorkUnit's identity/project/status and tuple key. Active Run capacity seeks strictly after the supplied expiration timestamp in the selected project's index; an exact TTL boundary is expired. Signed Unix seconds/nanoseconds preserve ordering beyond UnixNano's range. WorkUnit lifetime Run capacity includes closed/expired Runs and never exceeds the domain limit. These counts do not reserve capacity; the caller still owns its governance mutation lock and authorization.

WorkUnit accounting returns its native position, definition, Inflight and optional ID-sorted Runs. Missing/cross-project WorkUnit is successful absence only after a valid query. Inflight checks both accepted/unfinalized requests and pending attempts; finalized requests with unresolved attempts still count. Each selected existence row is joined to its real native-derived source and scope, not merely inferred from a bucket key. At most the domain lifetime limit (32) of Run definitions is returned, including legal wide native definitions without an artificial 8MiB page truncation.

Pending statistics read the canonical scalar count, first chronological pending index row and its real pending evidence in one pin. Count must be present, nonnegative and no greater than the pinned native sequence; zero/nonzero must agree with existence in both the chronological index and pending primary rows. The oldest timestamp and attempt identity/key must match the source. A future reservation has zero age. This is not a full pending-set recount or an audit of every unselected value.

## Bounded indexes, errors and ownership

The internal authenticated range walker retains one row and one MAC cursor, with a 4MiB row envelope and bounded per-query page count. Bucket ordinal plus fixed hash prefix and its lexicographic successor exclude neighboring buckets/scopes; strict-expiry start remains inside the selected project. Source JSON decoding is limited by the existing projection row maximum. Range/Get errors, dangling selected indexes and source mismatches fail the whole result, clearing scalar/detail output and found. Corruption retires every candidate reader/recovery view and Advance; quota/pressure/Busy/cancellation remain retryable.

Counts/statistics hold a 64MiB logical job workspace. Full WorkUnit accounting holds 128MiB for the bounded returned definitions and decoding; Snapshot's 32MiB query workspace and pin/bookkeeping are separately admitted by the same Manager. Caller retention of returned data needs its own outer budget. These floors are logical ownership, not RSS/cgroup acceptance. Closing/canceling uses the same tested point-job join and final publication checks; synchronous physical file reads remain non-interruptible while in progress.

Working memory does not retain a historical map/list. Project-wide counts still visit O(selected active/open rows), and each range/point lookup authenticates its tree path. This phase does not prove query IO/CPU deadlines or throughput for large active sets. Fixed snapshots, concurrent consumer aggregate limits, pin expiry/GC and physical calibration remain required.

## Validation and remaining conditions

[Local evidence](../verification/performance/2026-10-04-authenticated-accounting-read/README.md) covers fresh native/Bolt differential reads, each actual lifecycle cut, precise expiration, selected index corruption, retryable quotas/cancellation, and all 32 actual 900KiB native Run definitions against an independent full State oracle.

The subsequent [admission-reader contract](authenticated-ledger-admission-reader.md) completes lists, maturity and cohort. The complete method set remains a live component candidate. No new default Runtime consumer is selected. Cross-component proof/publication, native incremental Open, Metadata/legacy baselines, complete accepted-work/disk credits, Linux capacity and production acceptance remain open; I-11/M3 is not enabled.
