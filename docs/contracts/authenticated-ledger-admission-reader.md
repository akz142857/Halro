# Live authenticated Ledger admission reader

Status: component candidate, 2026-10-04. `AuthenticatedLedgerProjection` now satisfies the complete `AccountingAdmissionReader` method set. Construction still requires canonical reduction of a completely authenticated native source; no saved root, ordinary DB import, global Seal, Runtime selection or writer authority is introduced. Canonical reducer/key catalog/codec version are unchanged.

## Live lists and maturity

WorkUnit/Run pages use one authenticated pin per call, the same ID-ordered indexes and limits as the fresh Bolt reader. Scoped indexes bind project or WorkUnit, and each selected ID is joined to its real typed source. Filters include effective Run expiration at the supplied clock. Input limits, cumulative raw bytes (including filtered rows), returned rows and scanned rows are bounded. Sparse filtering may return an empty page with a continuation; a last scanned ID is not EOF. Continuations are live ID resume points, not saved-state/recovery proof.

A first row too wide for the byte allowance fails the whole page. A later wide row returns a continuation before that row, allowing a larger-budget retry. Required source rows, malformed/null JSON, scope/key/ID mismatch or physical authentication failure reject all output. No truncated valid prefix is published on corruption.

Maturity accepts at most 200 references/1MiB input. One pin fixes position, current WorkUnit definition/status and latest immutable cohort version at/before that position. The authenticated Floor result is checked for its bucket/hash prefix and sequence; no ordinary cursor supplies an authoritative hint. Cumulative definition/version bytes stay within 1MiB, including wrong-project definitions; bounded duplicate-reference caching avoids decoding the same wide definition 200 times and preserves input order. Missing/cross-project references are successful absence; missing/wrong version or status conflict is corruption. Caller retention of pages needs its own budget.

## Fixed cohort and visitor lifecycle

A whole cohort fixes one native-built authenticated pin and 64MiB logical workspace. Its finite batches retain minimal cohort rows; identity plus version inputs share the existing row/byte limits, and MaxWorkUnits counts eligible rows without accumulating a historical progress/ID map. The readonly authenticated row adapter reuses canonical version/unsigned 128-bit cost validators. Original creation identity determines eligibility, latest version at the pinned cut determines status, Inflight, known/estimated cost and unknown attempts. Overflow, excess eligible count and byte refusal return no successful head; any error invalidates all previously visited inputs.

Visitors run without owner/State locks or DB transactions. They may perform point reads or advance the actual native/candidate head; later batches still use the original pin. Recovery and cohort share one zero-queue callback admission slot. `WithRecoveryView` still requires both independent streams and Complete; cohort does not claim recovery completion.

Component Close cancels both modes and joins physical pins/active queries. It joins the complete recovery callback, as before. A cohort visitor may itself call component Close, so Close cannot also wait for that same visitor: the cohort workspace and callback slot remain owned until the visitor actually returns, and the whole query then fails instead of publishing its pinned head. External Close has the same visitor-fee behavior. A parent service/manager must retain its lifetime ticket through the last visitor and join that outer operation before retiring its own owner. Point-query decoding remains joined by the existing owner lock. None of these behaviors refunds a still-running visitor or grants reads after component Close.

Callback errors/cancellation/pressure/Busy/page limits are retryable on a healthy live owner. Semantic/source integrity failures and panic retire the candidate. Read failure retains its original cause. Physical file IO remains synchronous; this phase does not prove deadlines or RSS/cgroup peaks. Logical 64MiB job and 32MiB query fees are separate in the same Manager; wider WorkUnit detail remains 128MiB.

## Validation and remaining gates

[Local evidence](../verification/performance/2026-10-04-authenticated-admission-reader/README.md) covers fresh native/Bolt lists and maturity, bounded sparse filtering, legal 900KiB definitions with 200 duplicate references, visitor native Advance without mixed cuts, actual known/estimated/unknown cost plus closure against an independent State oracle, self/external Close and fee retention, and corruption/limit failures. Existing recovery completion/join behavior is also checked.

The separate [authenticated accounting writer](authenticated-ledger-accounting-writer.md)
now provides an explicit Manager constructor and serialized native-fact Apply.
This does not change Runtime defaults or enable authenticated snapshot startup.

The method set alone does not wire `AccountingMutationOwner`, trusted Runtime startup, budget admission/write ordering or default backend selection. Export/source APIs, cross-component proof, Metadata/legacy baselines, complete incremental native Open, physical disk/terminal credit ownership, pin deadlines/GC, Linux capacity and production acceptance remain open. I-11/M3 stays disabled.
