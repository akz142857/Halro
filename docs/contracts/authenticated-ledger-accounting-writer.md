# Authenticated Ledger accounting writer

`budget.NewWithAuthenticatedProjection` binds the complete accounting admission
reader and normal durable Apply to the same fresh authenticated projection and
native `ledger.Log`. It is an explicit constructor; Runtime defaults still use
their existing fresh Bolt projections. There is no saved-root opener.

Construction requires a live `MaintenanceLease` for that exact Log. Under
`WithHead`, `ValidateNativeWriter` checks the original native path and key tag,
candidate availability, and complete applied watermark/hash. Matching numeric
positions and hashes on another file do not establish source ownership. A
failed verification under the lease latches native recovery-required. Missing,
foreign, closed or already-canceled caller arguments refuse construction.

The caller must drain whole business requests/background producers, Retire/join
the old Manager, establish independently permitted role/generation and resource
ownership, finish request and attempt recovery and required confirmation, and
release maintenance before enabling business. Neither this constructor nor a
native durable head establishes those permissions. A second live mutation owner
or out-of-band Advance must not race the normal Manager.

`AdvanceOneFromNative` authenticates exactly the next semantic fact, including
physical Roll transitions, using the existing two frozen suffix passes. It
reduces actual native bytes, never a caller-supplied Event. The supplied prefix
must have exactly the next sequence and match the physical boundary/hash; skipped,
duplicate and forged cuts refuse without applying rows. The excluded durable
suffix is still authenticated. Saved roots, ordinary DB hints and this receipt
cannot authorize cross-process recovery, HA confirmation or a component Seal.

One durable caller may wait for existing component reads and the native writer.
An additional caller is refused with Busy, rather than queued. The source
workspace is charged before waiting and held through native verification,
component publication, lock release and cleanup. Close/Discard revoke admission
and join the admitted durable caller including fee cleanup. Existing generic
`AdvanceFromNative` remains a zero-wait candidate/maintenance operation.

The Manager retains its native mutation ticket and global Apply order. Already
accepted caller cancellation does not skip durable Apply. Native/component
shutdown or an Apply failure still fails closed, preserves the accepted native
fact and latches recovery-required; subsequent appends refuse before writing.
Local authenticated publication precedes observers, then the existing receipt's
required WaitConfirmed. Manager Retire joins observers/ACKs and clears its owner
references. Terminal shutdown withdraws admission, joins native Close and Manager
Retire before closing borrowed projection dependencies.

The 32MiB source workspace, existing 64MiB reduction/read workspaces and independent
query fees are logical bounds. They do not establish physical RSS/cgroup capacity,
IO deadlines, terminal disk/confirmation reserves, durable GC or pin deadlines.
Complete native startup, Runtime recovery/backend selection, all other
components, cross-component Seal and production acceptance remain separate work.

The explicit [two-level recovery adapter](authenticated-ledger-two-level-recovery.md)
now supports fixed original inputs and current native terminal evidence, using
the existing App role/term/full-ordering writer gates. Runtime backend/recovery
resource selection and authenticated snapshot startup remain separate work.

[Local evidence](../verification/performance/2026-10-04-authenticated-accounting-writer/README.md)
records the actual native Manager lifecycle, sequence/concurrency, confirmation,
cancellation and retirement checks, plus binding and charged-wait failures.
