# Authenticated accounting recovery resource owner

`Runtime.recoverAuthenticatedAccounting` is an explicit maintenance job. It
requires withdrawn admission, a native Log, a valid recovery role, configured
shared resources and the accounting owner's existing shared recovery slot.
`ValidateResourceOwner` requires the fresh authenticated candidate to use the
exact same Runtime Manager. An isolated component pool cannot satisfy this job.

The owner slot is a shared atomic pointer, also used by default Bolt recovery.
An alias Runtime does not obtain another queue or workspace. The same owner
refuses concurrent work immediately. The configured whole-job Recovery RAM
workspace is acquired before native writer construction or input enumeration.
It covers algorithm/encoding/confirmation lifetime; fixed input pins and actual
source/query/COW workspaces remain separately charged by the same Manager.

Normal return, error and panic cleanup first joins writer.Close and its actual
native operations, then retires the whole-job workspace, then clears the slot.
The Recovery view joins its own pin/callback cleanup before returning. Caller
cancellation while native fsync is blocked cannot release any of these scopes
or native exclusivity early. Persistent candidate disk/claim ownership remains
after job return and disappears only through actual component Discard.

Missing/foreign resource manager, missing configuration/slot, active business,
closed candidate, undersized descendant capacity or exhausted whole-job reserve
refuses before native mutation and does not take the caller's lease. Constructor
failure releases acquired job RAM and the slot. A successfully constructed
writer owns its existing Close/lease contract. A durable interrupted job still
latches native recovery-required; retry must use fresh authenticated native
construction and independently permitted source evidence.

Temporary source/query/COW operations now use callback-scoped authority from
one live RAM-only Recovery permit. Each IO/Background descendant is separately
charged to the same Manager's Recovery class; Unknown/Pressured does not stop
this explicit maintenance work. Ordinary business admission remains closed.
The scope rejects other classes, different Managers, disk/unconfirmed costs,
nested authority and escaped contexts. It retains the parent and joins final
retained/transferred child owners on return, cancellation or panic. Merging
claims cannot move fees across scopes. Closed Manager admission still refuses.

The explicit adapter checks a structural minimum before touching native writer
ownership: configured parent workspace (at least 128 MiB), 192 MiB descendant
workspaces, two 4096-byte snapshot pins, seven Recovery claims/control charges
and two owner slots. This is a lower bound, not RSS calibration or an aggregate
HA capacity guarantee; other live Recovery owners can still exhaust the pool.
Default Bolt configuration and startup selection are unchanged.

The original experiment used production-derived RAM-only limits and rejected
persistent COW admission. That is historical evidence: the current
`ResourceBudget.Limits` exposes configured total/class Disk limits and several
Runtime owners now hold logical file ceilings. Complete authoritative startup
inventory, durable COW ownership and physical acceptance remain incomplete. A
different private disk pool cannot bypass the same-owner gate. Cold candidate
construction and persistent ownership receive no scoped Recovery privilege.

The linked tests inject a RAM+Disk Manager explicitly and build a fresh private
candidate. Their successful admission does not establish current default
Runtime backend selection, complete startup reconciliation or calibrated
production capacity. The original RAM-only refusal test likewise describes its
then-current configuration, not today's configured Disk support.

This closes job ownership and cleanup for the explicit adapter, not Runtime
backend selection, full resource acceptance, physical RSS/cgroup bounds,
terminal/confirmation credit, persistent GC, pin deadlines, saved-root startup
or cross-component Seal. [Local evidence](../verification/performance/2026-10-04-authenticated-recovery-resource-owner/README.md)
records exact native IO, cancellation, fresh retry, refusal and validation results.

[Scoped Recovery evidence](../verification/performance/2026-10-04-recovery-scope/README.md)
records the new pressure eligibility and owner lifetime checks. Earlier linked
evidence describes the adapter before this addition.
