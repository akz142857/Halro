# Authenticated Ledger two-level recovery

`budget.RecoverAuthenticatedRequests` consumes one fixed native-built MAC root
through `WithRecoveryView`. The independent pending-attempt and unfinalized-request
streams must both reach Done and Complete. A request accepted before a lease is
still terminal work; a finalized request with a pending attempt is still attempt
work. No history-sized processed-ID map or full State participates in recovery.

`AuthenticatedRecoveryView.ReadRequest` reads original acceptance/legacy lease
origin and original finalization from that same pin. It validates domain identity
and sequence against the fixed head. New native admissions or settlements cannot
move this dependency to a later cut. Missing invented IDs are verified absence;
owner/corruption errors return no row or false absence. An escaped view is unusable.

The global original pending stream is settled first, using deterministic recovery
IDs, original lease metadata/period bounds, conservative started costs and the
existing unknown-price rules. Original finalizations remain unchanged. Each
nonempty settlement page must be confirmed before request finalization begins.

The request stream enumerates original lease membership, including already
settled attempts and empty membership. After native settlement Apply, the pinned
input still says Pending. Resolution therefore requires current authenticated
settlement evidence, original request/lease attribution, and an unchanged digest
for an originally settled attempt. It does not trust the old pending flag or an
in-memory list of IDs processed earlier. Nonempty finalization pages and the final
complete physical prefix are confirmed; even an empty retry has initial and final
confirmation. Results returned with an error are partial progress, not readiness.

The shared append verification checks the exact expected Event, next sequence,
physical watermark and chain hash. The explicit App authenticated writer reuses
the existing maintenance-only writer's role/term/identity lifetime authority,
complete ordering target (including earlier non-Ledger facts), post-append
authorization, failure latch and Close/join. Native application uses
`AdvanceOneFromNative`; arbitrary supplied rows are never accepted.

Cancellation/failure after native durability preserves the fact. Retry uses a
fresh candidate rebuilt from the newly authenticated, independently role-permitted
native prefix; this API cannot reopen a saved root, shrink a role's allowed cut or
publish a cross-component Seal. A durable settlement/finalization is not repeated.
Primary authority withdrawal refuses confirmation and further native append.

Pages remain at most 64 rows/8MiB. The fixed recovery callback's 64MiB logical job
workspace remains owned while it performs point joins, native writes and waits;
source/update/query workspaces have separate charges. Physical RSS/cgroup, IO
deadlines, terminal disk/confirmation reserves, GC/pin deadlines and complete
Runtime recovery-resource/backend selection are not established by this adapter.
The HA fixture uses an isolated explicit candidate budget, not a proof that all
production components share one enforced pool.

The explicit [Runtime recovery resource owner](authenticated-recovery-resource-owner.md)
now checks exact shared-Manager identity and owns the whole job/cleanup. Current
RAM-only production limits still block default backend/startup selection; its
shared Disk test pool is injected. Callback-scoped temporary Recovery authority
now permits the explicit maintenance job under Unknown/Pressured, with each
descendant separately charged and joined. Persistent construction is unchanged.

[Local evidence](../verification/performance/2026-10-04-authenticated-two-level-recovery/README.md)
covers actual native facts, multi-attempt membership, modern attributed and legacy
origins, interruption/fresh retry, empty confirmation and real mTLS Primary/Replica
confirmation followed by planned stepdown. It is not production acceptance.
