# Offline Usage verification from authenticated disk references

Status: local implementation, 2026-10-03. Actual `halro usage verify` now calls
`app.VerifyUsageAfter`; it does not build Aggregate or reconcile a Snapshot.
Default Runtime Usage, `prune` and `rebuild-summary` still use their existing
resident paths. Actual [offline compact](usage-offline-compaction.md) now shares
the fresh disk reference and authority/cleanup barrier. I-08 remains ongoing.

## Command and interval

Run with the server stopped and the existing data-directory lock available:

```text
halro usage verify --config ./config.yaml
halro usage verify --config ./config.yaml --after-sequence 12345
```

Default verification compares native-derived attempts from genesis through the
captured archive H. The optional unsigned `--after-sequence` explicitly selects
`(after, H]`; it does not establish a retention policy or derive a floor from
missing partitions. An invalid floor fails. The flag belongs only to `verify`.
Both legacy JSON and paged catalog v1, Parquet and NDJSON use the same
[exact archive verifier](usage-exact-archive-verification.md), without migrating
the published catalog. Date-retention coverage still requires a separate proof.

## Independent native authority

Before creating any derivative or exporter, the operation obtains the existing
exclusive directory lock, opens exact-schema Metadata read-only, unlocks and
checks the Vault key, and loads the native Ledger authentication key. It fully
authenticates the native chain, including sealed/gzip history and excluded tails.
The Metadata Ledger checkpoint must match its actual historical hash and offset,
including when it precedes the current native head. Metadata's applied position
and transaction identity are captured independently of a candidate projection.
No Metadata migration, native repair, tail truncation, native sync, old Usage
checkpoint/rollup rewrite or native deletion is performed.

An existing member cannot become Standalone by removing replication config.
Configured membership must already exist; verification cannot create or join it.
Signed member identity and configuration must agree; reseed-required and
AwaitingDecision members are refused. Role selection is explicit:

| Role | Ledger reference cut |
| --- | --- |
| Standalone | Complete authenticated physical native head |
| Primary | Authenticated member durable ordering index |
| Replica | Authenticated member applied ordering index |

Primary's durable cut and quorum confirmation remain distinct: this report does
not grant readiness, promotion or permission to commit a business result.
Replica also requires its signed Metadata projection to agree with both the
actual Metadata applied position and the selected ordering Metadata cursor.

[Read-only ordering inspection](ordering-read-only-inspection.md) authenticates
the complete physical journal and preserves allowed, durable and physical cuts.
`InspectOrderingLedger` binds the seeded Ledger baseline head, every Ledger
range's exact reconstructed frame digest, and every Ledger roll's actual native
metadata/digest. It checks excluded ordered tails as well. The authenticated
durable term must match the member term. Other stores' native bodies are outside
this Usage-specific inspector; their ordering MACs and transitions still validate.
The selected logical cursor resolves to an exact physical Ledger prefix,
including empty roll boundaries. Equal sequences alone cannot supply authority.

## Candidate ownership and final checks

Only after authority validation, two fresh private Ledger/Usage disk projections
are built under a new temporary directory outside the native data directory.
Existing derived DBs are never trusted or reused. Ledger receives the exact
role-allowed physical prefix; Usage replays the complete authenticated source
but applies only facts through that prefix. A repeated source report must match
the frozen report, and Usage must reach the allowed semantic sequence without
passing its physical head. The same Ledger reader supplies derivative semantics.

After archive comparison, native authentication, historical checkpoint,
Metadata applied/transaction evidence, member identity/role/cuts and the full
native-bound ordering scan are checked again. The named Metadata file must
retain its captured regular inode, size and modification time. Context and
membership guards are checked before publication. Any changed authority,
authentication error, limit, cancellation or cleanup error clears the entire
report; candidate agreement cannot certify a changed source.

Cleanup joins derivative work and closes both physical DBs before removing only
the matching owned temporary directory; then keys are cleared and Metadata and
the external lock close. Context is checked again after actual cleanup; cancellation
during a blocked DB close also withdraws the report after its reader exits.
A replaced temporary directory is preserved and fails
the operation. Crashes can leave scratch; orphan reconciliation is unfinished.
The archive verifier also owns temporary index/spool files in the Usage tree,
so the operation preserves existing files but is not filesystem-wide read-only.

## JSON scope and remaining work

Successful output retains `ledger_records`, `parquet_records`, `missing`,
`duplicates` and `extra`; the latter three are zero. `parquet_records` is the
compatibility name for archive rows, including NDJSON. Scope is explicit:

| Field | Meaning |
| --- | --- |
| `after_sequence`, `through_sequence` | Selected interval floor and archive H |
| `source_head` | Fresh Usage's last actual processed event position |
| `allowed_ledger_head` | Exact role-allowed physical Ledger cut, including rolls |
| `native_head` | Complete authenticated physical native tail |
| `role`, `ordering_index` | Selected role and allowed global ordering cut; index zero for Standalone or a seeded zero cut |

Native/allowed/source/archive positions need not coincide. The report proves
exact relative row correspondence in its named interval, not that every current
fact is archived, D-03 content/key-set trust, or pruning permission.

The whole operation has a cooperative one-minute context; archive decoding has
its own finite file/row/byte/index ceilings. Native rebuild disk/mmap use is not
yet charged to shared physical budgets. Complete authentication is repeated,
and each historical roll lookup still authenticates native history; total IO
can grow with roll count times history. This slice does not establish indexed
startup, a hard filesystem deadline, resumable whole-history verification,
authenticated snapshot reopening or Linux/cgroup capacity. Default Usage
startup, prune, retention/migration/crash recovery,
remaining shared fees, I-12 indexing and release acceptance remain open.

Local source-cut and gate evidence: [verification record](../verification/performance/2026-10-03-usage-offline-disk-verification/README.md).

The actual [summary rebuild](usage-summary-disk-rebuild.md) also uses this fresh
reference, with an offline Standalone writable publication after all independent
checks and readonly Metadata close. Its exact physical-file/authority handoff and
post-publication fences do not change Verify or Compact into writable Metadata
operations.
