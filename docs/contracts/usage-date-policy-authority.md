# Date policy activation authority

Local implementation, 2026-10-03. This addresses the independently reproduced
[original-baseline authorization defect](../verification/performance/2026-10-03-usage-date-data-reclaim/README.md).
The [final local gates](../verification/performance/2026-10-03-usage-date-policy-authority/README.md) are recorded separately from that historical failing probe.

## Authenticated decision

The fixed activation file now supports state version 2 with one policy receipt.
Its domain-separated HMAC covers SHA-256 of the exact old and canonical new root
bytes plus the exact original native `ProjectionPrefix` (physical generation,
offset, sequence and chain hash). Root bytes bind both the explicit retention
date and original prune baseline, schemas, immutable page references and counters.
The signed baseline cannot exceed the authenticated original source position.
The derived `intent` / `published` phase is excluded from the signature so crash
recovery can repeat durability barriers without minting new policy authority.

The receipt is signed only for a new missing obligation, after the fresh complete
candidate comparison and caller's independent publication checks. The existing
publisher writes and synchronizes the signed intent, closes the actual temporary
file, renames and synchronizes its directory before publishing the new root.
Existing obligations must already authenticate and match the same exact root
pair; they cannot silently acquire a signature from the recovery path.

## Independent native source and lifetime

Exporter options require both `DatePolicyKey` and `DatePolicyPrefix` for policy
activation/reclamation. Ordinary export and read-only construction need neither.
The supplied key must come from independently unlocked native authority, never
an archive file, saved DB or checkpoint. Exporter retains only a fixed 32-byte
HMAC-derived key under a distinct policy-key domain. Constructor failure erases
that key; Close permanently retires work, joins accepted physical jobs and source
resolvers, then erases it and releases the resolver reference.

The resolver must authenticate complete native history under exclusive ownership,
enforce the independently selected role scope, and return the exact requested
physical position/hash. The App offline factory does this with
`InspectProjectionCursor`, comparing its complete report to the independently
frozen native source and refusing a position beyond the selected allowed prefix.
On reopen, valid HMAC alone is insufficient: the original prefix must still exist
with the same physical position and native hash in that fresh authenticated source.
Published-root comparison checks this before classifying any permitted omission
or minting a deletion proof. Activation retry and each physical reclaim call also
check it. A different validly authenticated native branch cannot reuse the receipt.

These source inspections use fixed memory, but still scan history under the
existing operation deadline. They are not resumable native initialization, and
their IO is separate from the finite descriptor/file comparison budgets. No
bounded startup-time or Linux capacity claim follows from this change.

## Refusal and retained obligations

Unsigned version 1, missing/changed signature, changed date/baseline/root pair,
wrong independently loaded key, unavailable original prefix or changed native
hash fail closed. No existing unsigned obligation is backfilled. Restoring or
rebuilding a legacy policy needs an explicit migration protocol; this is not yet
implemented. Refusal does not delete an archive object, mutate native facts,
complete the obligation or publish a success report.

The fixed signed decision must survive for the entire lifetime of the active
retention policy. Currently it remains in the retained activation obligation.
[Finite original-page reclamation](usage-date-page-reclamation.md) now preserves
the decision and uses separate authenticated irreversible transition evidence.
Future orphan reclamation and obligation completion must preserve this
authenticated decision before retiring the remaining enumeration state. They must not
erase the only receipt while the active root still relies on its original baseline.

The receipt authenticates a policy decision only. Saved progress, Ready, private
indexes and old-root enumeration still require their existing finite reconstruction
and physical checks. It grants no saved-projection trust, native trim, shared
physical-fee refund, HA confirmation or promotion authority. Offline activation
and physical reclamation continue to refuse HA and persistent-member downgrade.
CLI/default Runtime wiring, authenticated snapshots, native initialization resume,
page/orphan/obligation completion and production acceptance remain separate work.

The local [explicit orphan-file operation](usage-date-orphan-file-reclamation.md)
also verifies this original decision on every call. Genuine native rows newer
than the signed baseline remain protected even when their dates look expired.
Neither a newly derived object checksum nor a historical signed page Ready can
advance this baseline or complete the obligation.

## Persistent current-policy follow-up

[Persistent current date policy](usage-date-active-policy.md) can now preserve
one authentic historical original-chain result and its independently native-bound
policy in a bounded slot. Published verification/reclaim/retry can authenticate
the matching current policy even if the original marker is absent or holds a
future intent. This copy leaves orphans pending and preserves original files;
normal next-policy/transition succession and full obligation closure remain open.
