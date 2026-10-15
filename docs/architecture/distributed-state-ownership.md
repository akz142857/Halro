# Distributed state ownership

This matrix is the Phase 0 contract for future HA and Cluster work. It does not
change the Standalone runtime described by ADR 0001.

| State | Current authority | Scope | Durability/order | Failure policy | Future HA treatment |
|---|---|---|---|---|---|
| Credentials, Providers, Deployments, Routes | bbolt metadata | global or deployment | transactional revision order | fail closed | replicated control-plane mutations |
| Projects and Gateway keys | bbolt metadata | Project | transactional revision order | fail closed | owned by Project shard; revocation ordered |
| Budget reservations and settlements | Ledger WAL | Project/day | framed append, sequence, fsync | fail closed; unknown calls settle conservatively | shard replicated log before Provider I/O |
| Request and Attempt lifecycle | Ledger WAL | Project/request | Ledger sequence | fail closed | one shard owner and ownership epoch |
| Audit chain and checkpoint | audit log plus bbolt head | global/shard | HMAC chain order | readiness degradation/fail closed for mutation | per-shard chain with exported cluster view |
| Auth snapshot | rebuildable from bbolt | node cache | applied metadata revision | reject on unavailable authority | versioned apply from owning shard |
| Provider route registry | rebuildable from bbolt | node cache | applied metadata revision | fail closed if target is ambiguous | versioned apply; no independent authority |
| RPM/TPM/concurrency counters | in-memory enforcement | Project | rolling-window order | reject at limit | authoritative on Project owner |
| Token Guard fixed/block state | bbolt policy plus checkpoint/runtime | Project | policy revision and checkpoint | fail closed for fixed blocking rules | authoritative on Project owner |
| Token Guard EWMA | rebuildable checkpoint/runtime | Project | policy revision and sample order | detect-only fallback | owner-computed derivative |
| Circuit breaker observations | in-memory | node/deployment | local observation order | local target avoidance | deliberately node-local |
| Provider connection pools | in-memory | node | none | reconnect | deliberately node-local |
| Usage aggregate/checkpoint | Ledger derivative | shard/node | Ledger watermark | catch up from Ledger | rebuild from replicated Ledger |
| Parquet analytics | Ledger derivative | shard/day | manifest and watermark | rebuild/repair | shard export then federated query |
| Admin sessions | bbolt metadata | control plane | transactional | fail closed | replicated or leader-bound with rotation |
| Alert delivery queue | in-memory derivative; config in bbolt | node | bounded retry/dedup | drop is observable | owner emits; delivery remains node-local |
| Backups | metadata/Ledger/usage/audit snapshot | shard | fixed epoch and watermarks | offline verification required | shard-consistent snapshot plus directory manifest |
| Live HTTP/SSE connections | process memory | node/request | connection lifetime | terminate on owner loss | never replicated or migrated |
| Metrics | in-memory derivative | node | none | scrape gap acceptable | node metrics plus cluster aggregation |

## Invariants

1. Exactly one ownership epoch may start authoritative work for a Project.
2. Budget reservation is committed before a Provider can receive a request.
3. A stale ownership token cannot commit or begin external side effects.
4. Derived state never changes an authoritative Ledger balance.
5. Replaying the same canonical mutations produces the same authoritative state.
6. Ambiguous ownership, unsupported schema, lost quorum, and corrupt authority
   fail closed.
7. Active streams terminate on owner loss; they are not transferred.
8. Idempotency suppresses duplicate Halro execution where knowable, but does
   not turn an upstream unknown outcome into exactly-once execution.
